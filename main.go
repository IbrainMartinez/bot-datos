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

	"bot-datos/config"

	"github.com/PuerkitoBio/goquery"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"nhooyr.io/websocket"
)

// StatusResponse define la estructura del JSON que se enviará.
type StatusResponse struct {
	LastUpdate string `json:"last_update"`
	TotalCount int64  `json:"total_count"`
	URL        string `json:"url,omitempty"`
	ID         string `json:"id,omitempty"`
	Op         string `json:"op,omitempty"`
}

// Link representa un documento en la colección con una URL.
type Link struct {
	ID  primitive.ObjectID `bson:"_id" json:"id"`
	URL string             `bson:"url" json:"url"`
}

type Client struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

var (
	clients   = make(map[*Client]bool)
	clientsMu sync.Mutex

	mu         sync.Mutex
	lastUpdate time.Time
	totalCount int64
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mongoClient, err := mongo.Connect(ctx, options.Client().ApplyURI(config.MongoURI))
	if err != nil {
		log.Fatal("Error conectando a MongoDB:", err)
	}
	defer mongoClient.Disconnect(ctx)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	server := &http.Server{
		Addr: ":" + config.WebsocketPort,
	}

	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/links", linksHandler(mongoClient))
	http.HandleFunc("/preview", previewHandler) // nuevo endpoint
	http.Handle("/", http.FileServer(http.Dir("./frontend")))

	go func() {
		log.Printf("Servidor WebSocket escuchando en puerto :%s\n", config.WebsocketPort)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error del servidor: %v", err)
		}
	}()

	go startChangeStream(ctx, mongoClient)

	<-sigs
	log.Println("Apagando...")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	server.Shutdown(shutdownCtx)
	cancel()

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
		log.Println("Error al aceptar WS:", err)
		return
	}

	client := &Client{conn: conn}
	clientsMu.Lock()
	clients[client] = true
	clientsMu.Unlock()

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
	_ = conn.Close(websocket.StatusNormalClosure, "bye")
}

func sendInitialStatus(client *Client) {
	mu.Lock()
	defer mu.Unlock()
	lu := ""
	if !lastUpdate.IsZero() {
		lu = lastUpdate.Format(time.RFC3339)
	}
	initialStatus := StatusResponse{
		LastUpdate: lu,
		TotalCount: totalCount,
	}
	sendJSON(context.Background(), client, initialStatus)
}

func sendJSON(ctx context.Context, client *Client, data StatusResponse) {
	client.writeMu.Lock()
	defer client.writeMu.Unlock()
	w, err := client.conn.Writer(ctx, websocket.MessageText)
	if err != nil {
		return
	}
	defer w.Close()
	_ = json.NewEncoder(w).Encode(data)
}

func startChangeStream(ctx context.Context, client *mongo.Client) {
	collection := client.Database(config.MongoDBName).Collection(config.MongoCollection)
	initialCount, err := collection.CountDocuments(ctx, bson.M{})
	if err != nil {
		log.Fatal(err)
	}
	mu.Lock()
	totalCount = initialCount
	mu.Unlock()

	opts := options.ChangeStream().SetFullDocument(options.UpdateLookup)
	changeStream, err := collection.Watch(ctx, mongo.Pipeline{}, opts)
	if err != nil {
		log.Fatal(err)
	}
	defer changeStream.Close(ctx)

	for changeStream.Next(ctx) {
		var changeDoc bson.M
		if err := changeStream.Decode(&changeDoc); err != nil {
			continue
		}

		mu.Lock()
		lastUpdate = time.Now()
		status := StatusResponse{LastUpdate: lastUpdate.Format(time.RFC3339)}
		switch changeDoc["operationType"].(string) {
		case "insert":
			totalCount++
			status.Op = "insert"
			if fullDoc, ok := changeDoc["fullDocument"].(bson.M); ok {
				if url, ok := fullDoc["url"].(string); ok {
					status.URL = url
				}
				if id, ok := fullDoc["_id"].(primitive.ObjectID); ok {
					status.ID = id.Hex()
				}
			}
		case "delete":
			if totalCount > 0 {
				totalCount--
			}
			status.Op = "delete"
			if docKey, ok := changeDoc["documentKey"].(bson.M); ok {
				if id, ok := docKey["_id"].(primitive.ObjectID); ok {
					status.ID = id.Hex()
				}
			}
		}
		status.TotalCount = totalCount

		clientsMu.Lock()
		for c := range clients {
			go func(cl *Client, snap StatusResponse) {
				sendJSON(ctx, cl, snap)
			}(c, status)
		}
		clientsMu.Unlock()
		mu.Unlock()
	}
}

func linksHandler(client *mongo.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		coll := client.Database(config.MongoDBName).Collection(config.MongoCollection)
		ctx := r.Context()
		cursor, err := coll.Find(ctx, bson.M{})
		if err != nil {
			http.Error(w, "DB error", http.StatusInternalServerError)
			return
		}
		defer cursor.Close(ctx)

		var links []Link
		if err := cursor.All(ctx, &links); err != nil {
			http.Error(w, "decode error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(links)
	}
}

// Nuevo: endpoint para obtener la imagen OG
func previewHandler(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Query().Get("u")
	if url == "" {
		http.Error(w, "missing u", http.StatusBadRequest)
		return
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; LinkPreviewBot/1.0)")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "fetch error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		http.Error(w, "parse error", http.StatusBadGateway)
		return
	}
	og := ""
	doc.Find(`meta[property="og:image"]`).Each(func(i int, s *goquery.Selection) {
		if v, ok := s.Attr("content"); ok && og == "" {
			og = v
		}
	})
	if og == "" {
		http.Error(w, "no image", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(og))
}
