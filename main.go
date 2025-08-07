package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"bot-datos/config" // Ajusta según tu módulo

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"nhooyr.io/websocket"
)

// StatusResponse define la estructura del JSON que se enviará.
type StatusResponse struct {
	LastUpdate string `json:"last_update"`
	TotalCount int64  `json:"total_count"`
}

// Link representa un documento en la colección con una URL.
type Link struct {
	URL string `bson:"url" json:"url"`
}

// Cliente WebSocket con mutex de escritura para evitar escrituras concurrentes
type Client struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

// Globales para el estado y los clientes conectados
var (
	clients   = make(map[*Client]bool) // Mapa para registrar clientes
	clientsMu sync.Mutex               // Mutex para proteger el mapa de clientes

	mu         sync.Mutex // Mutex para proteger el estado del servicio
	lastUpdate time.Time
	totalCount int64
)

func main() {
	// Contexto raíz cancelable para control de apagado
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Conexión a MongoDB reutilizable
	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI(config.MongoURI))
	if err != nil {
		log.Fatal("Error conectando a MongoDB:", err)
	}
	defer mongoClient.Disconnect(ctx)

	// Señales del SO para apagado ordenado
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	// Servidor HTTP con apagado ordenado
	server := &http.Server{
		Addr: ":" + config.WebsocketPort,
	}

	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/links", linksHandler(mongoClient))
	http.Handle("/", http.FileServer(http.Dir("./frontend")))

	go func() {
		log.Printf("Servidor WebSocket escuchando en puerto :%s en el endpoint /ws\n", config.WebsocketPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error del servidor: %v", err)
		}
	}()

	// Change stream
	go startChangeStream(ctx, mongoClient)

	// Espera señal
	<-sigs
	log.Println("Recibida señal de apagado. Cerrando...")

	// Cierra servidor HTTP
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Error al apagar servidor HTTP: %v", err)
	}

	// Cancela contexto para terminar change stream
	cancel()

	// Cierra conexiones de clientes
	clientsMu.Lock()
	for c := range clients {
		_ = c.conn.Close(websocket.StatusNormalClosure, "Servidor apagándose")
		delete(clients, c)
	}
	clientsMu.Unlock()

	log.Println("Apagado completo.")
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	options := &websocket.AcceptOptions{
		OriginPatterns: config.AllowedOrigins,
	}

	conn, err := websocket.Accept(w, r, options)
	if err != nil {
		log.Println("Error al aceptar la conexión WebSocket:", err)
		return
	}

	client := &Client{conn: conn}
	clientsMu.Lock()
	clients[client] = true
	clientsMu.Unlock()

	log.Printf("Nuevo cliente conectado. Total: %d\n", len(clients))

	sendInitialStatus(client)

	ctx := r.Context()
	for {
		_, _, err := conn.Read(ctx)
		if err != nil {
			break
		}
	}

	clientsMu.Lock()
	delete(clients, client)
	clientsMu.Unlock()

	conn.Close(websocket.StatusInternalError, "Conexión cerrada")
	log.Printf("Cliente desconectado. Total: %d\n", len(clients))
}

func sendInitialStatus(client *Client) {
	mu.Lock()
	defer mu.Unlock()
	initialStatus := StatusResponse{
		LastUpdate: lastUpdate.Format(time.RFC3339),
		TotalCount: totalCount,
	}
	ctx := context.Background()
	sendJSON(ctx, client, initialStatus)
}

// Función corregida para enviar JSON
func sendJSON(ctx context.Context, client *Client, data StatusResponse) {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()

	w, err := client.conn.Writer(ctx, websocket.MessageText)
	if err != nil {
		log.Println("Error al obtener el escritor de WebSocket:", err)
		return
	}
	defer w.Close()

	if err := json.NewEncoder(w).Encode(data); err != nil {
		log.Println("Error al codificar y enviar JSON:", err)
	}
}

func startChangeStream(ctx context.Context, client *mongo.Client) {

	dbName := config.MongoDBName
	collName := config.MongoCollection
	collection := client.Database(dbName).Collection(collName)
	initialCount, err := collection.CountDocuments(ctx, bson.M{})
	if err != nil {
		log.Fatal("Error obteniendo el conteo inicial:", err)
	}
	mu.Lock()
	totalCount = initialCount
	mu.Unlock()
	log.Printf("Conteo inicial de documentos: %d\n", initialCount)

	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup)
	changeStream, err := collection.Watch(ctx, mongo.Pipeline{}, opts)
	if err != nil {
		log.Fatal("Error creando Change Stream:", err)
	}
	defer changeStream.Close(ctx)

	log.Printf("Escuchando cambios en la colección '%s.%s'...\n", dbName, collName)

	for changeStream.Next(ctx) {
		var changeDoc bson.M
		if err := changeStream.Decode(&changeDoc); err != nil {
			log.Println("Error decodificando cambio:", err)
			continue
		}

		mu.Lock()
		lastUpdate = time.Now()
		switch changeDoc["operationType"].(string) {
		case "insert":
			totalCount++
		case "delete":
			if totalCount > 0 {
				totalCount--
			}
		}

		status := StatusResponse{
			LastUpdate: lastUpdate.Format(time.RFC3339),
			TotalCount: totalCount,
		}

		clientsMu.Lock()
		for c := range clients {
			// envío secuencial por cliente protegido por su mutex interno
			go func(clientRef *Client, snapshot StatusResponse) {
				sendJSON(ctx, clientRef, snapshot)
			}(c, status)
		}
		clientsMu.Unlock()

		mu.Unlock()
	}

	if err := changeStream.Err(); err != nil {
		log.Fatal("Error en el Change Stream:", err)
	}
}

// linksHandler devuelve todas las URLs almacenadas en la base de datos en formato JSON.
func linksHandler(client *mongo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		coll := client.Database(config.MongoDBName).Collection(config.MongoCollection)
		ctx := r.Context()
		cursor, err := coll.Find(ctx, bson.M{})
		if err != nil {
			http.Error(w, "error al consultar la base de datos", http.StatusInternalServerError)
			return
		}
		defer cursor.Close(ctx)

		var links []Link
		if err := cursor.All(ctx, &links); err != nil {
			http.Error(w, "error al decodificar resultados", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(links); err != nil {
			log.Println("Error enviando JSON:", err)
		}
	}
}
