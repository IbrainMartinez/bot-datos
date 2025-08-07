// config/config.go
package config

import (
    "log"
    "os"

    "github.com/joho/godotenv"
)

var MongoURI string
var MongoDBName string
var MongoCollection string
var WebsocketPort string
var AllowedOrigins []string

func init() {
    // Carga variables desde .env del proyecto (en la raíz)
    err := godotenv.Load()
    if err != nil {
        log.Println("No se pudo cargar archivo .env, asegúrate que existe o variables están configuradas")
    }

    // Lee la variable y la guarda en la variable global del paquete
    MongoURI = os.Getenv("MONGO_BOT")
    if MongoURI == "" {
        log.Fatal("MONGO_BOT no está configurada")
    }

    MongoDBName = getEnvDefault("MONGO_DB", "test")
    MongoCollection = getEnvDefault("MONGO_COLLECTION", "urls")

    WebsocketPort = getEnvDefault("WS_PORT", "8080")

    // Orígenes permitidos para WebSocket (separados por coma)
    originsEnv := getEnvDefault("WS_ALLOWED_ORIGINS", "127.0.0.1:5500,localhost:5500")
    AllowedOrigins = splitAndTrim(originsEnv)
}

func getEnvDefault(key, defaultVal string) string {
    v := os.Getenv(key)
    if v == "" {
        return defaultVal
    }
    return v
}

func splitAndTrim(csv string) []string {
    var out []string
    start := 0
    for i := 0; i <= len(csv); i++ {
        if i == len(csv) || csv[i] == ',' {
            token := csv[start:i]
            // trim spaces
            token = trimSpaces(token)
            if token != "" {
                out = append(out, token)
            }
            start = i + 1
        }
    }
    if len(out) == 0 {
        return []string{"127.0.0.1:5500"}
    }
    return out
}

func trimSpaces(s string) string {
    // simple local trim to avoid importing strings
    for len(s) > 0 && (s[0] == ' ' || s[0] == '\t' || s[0] == '\n' || s[0] == '\r') {
        s = s[1:]
    }
    for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t' || s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
        s = s[:len(s)-1]
    }
    return s
}
