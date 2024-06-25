package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

type connection struct {
	fileName    string
	file        *os.File
	mu          sync.Mutex
	dataWritten uint32
	binaryQueue chan []byte
}
const (
	sampleRate    = 8000
	bitsPerSample = 16
	numChannels   = 1
	wavHeaderSize = 44
	maxFileSize   = 4294967295 // max value for uint32
)

var (
	s3Endpoint        string
	s3AccessKeyID     string
	s3SecretAccessKey string
	s3BucketName      string
	s3UseSSL          bool
	s3Prefix          string
	serverPort        string
)

func init() {
	if err := godotenv.Load(); err != nil {
		slog.Error("Error loading .env file", "error", err)
	}

	s3Endpoint = os.Getenv("S3_ENDPOINT")
	s3AccessKeyID = os.Getenv("S3_ACCESS_KEY_ID")
	s3SecretAccessKey = os.Getenv("S3_SECRET_ACCESS_KEY")
	s3BucketName = os.Getenv("S3_BUCKET_NAME")
	s3UseSSL = os.Getenv("S3_USE_SSL") == "true"
	s3Prefix = os.Getenv("S3_PREFIX")
	serverPort = os.Getenv("SERVER_PORT")
}

func main() {
	logLevel := slog.LevelInfo
	if os.Getenv("LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	slog.SetDefault(logger)

	slog.Info("Starting WebSocket server", "port", serverPort)
	http.HandleFunc("/ws", handleWebSocket)
	slog.Info("Server is ready to accept connections")
	if err := http.ListenAndServe(":"+serverPort, nil); err != nil {
		slog.Error("Server stopped", "error", err)
	}
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("Error upgrading to WebSocket", "error", err)
		return
	}
	defer conn.Close()

	c := &connection{
		binaryQueue: make(chan []byte, 100), // Buffer for 100 binary messages
	}
	defer c.closeFileAndUpload()

	go c.processBinaryQueue()

	slog.Info("WebSocket connection established", "remoteAddr", conn.RemoteAddr())

	for {
		messageType, reader, err := conn.NextReader()
		if err != nil {
			slog.Debug("Connection closed", "error", err)
			return
		}

		switch messageType {
		case websocket.TextMessage:
			if err := c.handleTextMessage(reader); err != nil {
				slog.Error("Error handling text message", "error", err)
				return
			}
		case websocket.BinaryMessage:
			if err := c.handleBinaryMessage(reader); err != nil {
				slog.Error("Error handling binary message", "error", err)
				return
			}
		default:
			slog.Warn("Unexpected message type", "type", messageType)
		}
	}
}

func (c *connection) handleTextMessage(reader io.Reader) error {
	fileNameBytes, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("error reading filename: %w", err)
	}

	newFileName := strings.TrimSpace(string(fileNameBytes))
	if newFileName == "" {
		return fmt.Errorf("received empty filename")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Close and upload the previous file if it exists
	c.closeFileAndUpload()

	// Create a new file
	c.fileName = newFileName
	c.file, err = os.Create(c.fileName)
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", c.fileName, err)
	}

	if err := c.writeWAVHeader(); err != nil {
		return fmt.Errorf("error writing WAV header: %w", err)
	}
	c.dataWritten = 0

	slog.Debug("New file created", "filename", c.fileName)
	return nil
}

func (c *connection) handleBinaryMessage(reader io.Reader) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("error reading binary data: %w", err)
	}

	slog.Debug("Received binary data", "bytes", len(data))

	if len(data) == 0 {
		slog.Warn("Received empty binary message")
		return nil
	}

	c.binaryQueue <- data
	return nil
}



func (c *connection) processBinaryQueue() {
	for data := range c.binaryQueue {
		c.mu.Lock()
		if c.file == nil {
			slog.Warn("No file open, discarding binary data", "dataSize", len(data))
			c.mu.Unlock()
			continue
		}

		slog.Debug("Processing binary data", "filename", c.fileName, "dataSize", len(data))

		if _, err := c.file.Seek(0, io.SeekEnd); err != nil {
			slog.Error("Error seeking to end of file", "error", err, "filename", c.fileName)
			c.mu.Unlock()
			continue
		}

		bytesWritten, err := c.file.Write(data)
		if err != nil {
			slog.Error("Error writing to file", "error", err, "filename", c.fileName)
			c.mu.Unlock()
			continue
		}

		c.dataWritten += uint32(bytesWritten)
		slog.Debug("Data written to file", "bytesWritten", bytesWritten, "totalDataSize", c.dataWritten)

		if c.dataWritten > maxFileSize-wavHeaderSize {
			slog.Warn("Maximum file size reached", "filename", c.fileName)
		}

		if err := c.file.Sync(); err != nil {
			slog.Error("Error syncing file", "error", err, "filename", c.fileName)
		}

		if err := c.updateWAVHeader(); err != nil {
			slog.Error("Error updating WAV header", "error", err, "filename", c.fileName)
		}

		c.mu.Unlock()
	}
}

func (c *connection) updateWAVHeader() error {
	fileInfo, err := c.file.Stat()
	if err != nil {
		return fmt.Errorf("error getting file info: %w", err)
	}
	fileSize := fileInfo.Size()

	slog.Debug("Updating WAV header", "filename", c.fileName, "fileSize", fileSize, "dataSize", c.dataWritten)

	headerUpdates := make([]byte, 8)
	binary.LittleEndian.PutUint32(headerUpdates[0:4], uint32(fileSize-8))
	binary.LittleEndian.PutUint32(headerUpdates[4:8], uint32(c.dataWritten))

	if _, err := c.file.Seek(4, io.SeekStart); err != nil {
		return fmt.Errorf("error seeking to update file size: %w", err)
	}
	if _, err := c.file.Write(headerUpdates[0:4]); err != nil {
		return fmt.Errorf("error updating file size: %w", err)
	}

	if _, err := c.file.Seek(40, io.SeekStart); err != nil {
		return fmt.Errorf("error seeking to update data size: %w", err)
	}
	if _, err := c.file.Write(headerUpdates[4:8]); err != nil {
		return fmt.Errorf("error updating data size: %w", err)
	}

	if _, err := c.file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("error seeking to end of file: %w", err)
	}

	return c.file.Sync()
}
func (c *connection) writeWAVHeader() error {
	header := make([]byte, wavHeaderSize)

	// RIFF header
	copy(header[0:4], []byte("RIFF"))
	binary.LittleEndian.PutUint32(header[4:8], uint32(wavHeaderSize-8))
	copy(header[8:12], []byte("WAVE"))

	// fmt subchunk
	copy(header[12:16], []byte("fmt "))
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], numChannels)
	binary.LittleEndian.PutUint32(header[24:28], sampleRate)
	binary.LittleEndian.PutUint32(header[28:32], sampleRate*numChannels*bitsPerSample/8)
	binary.LittleEndian.PutUint16(header[32:34], numChannels*bitsPerSample/8)
	binary.LittleEndian.PutUint16(header[34:36], bitsPerSample)

	// data subchunk
	copy(header[36:40], []byte("data"))
	binary.LittleEndian.PutUint32(header[40:44], 0)

	_, err := c.file.Write(header)
	if err != nil {
		return fmt.Errorf("error writing WAV header: %w", err)
	}

	return c.file.Sync()
}



func (c *connection) closeFileAndUpload() {
	if c.file != nil {
		if err := c.file.Close(); err != nil {
			slog.Error("Error closing file", "error", err, "filename", c.fileName)
			return
		}

		if err := s3Upload(c.fileName, filepath.Base(c.fileName), s3Prefix); err != nil {
			slog.Error("Error uploading file to S3", "error", err, "filename", c.fileName)
		} else {
			slog.Info("Successfully uploaded file to S3", "filename", c.fileName)
			if err := os.Remove(c.fileName); err != nil {
				slog.Error("Error removing local file", "error", err, "filename", c.fileName)
			}
		}

		c.file = nil
		c.fileName = ""
		c.dataWritten = 0
	}
}
func s3Upload(filePath, objectName, prefix string) error {
	ctx := context.Background()

	minioClient, err := minio.New(s3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(s3AccessKeyID, s3SecretAccessKey, ""),
		Secure: s3UseSSL,
	})
	if err != nil {
		return fmt.Errorf("error creating S3 client: %w", err)
	}

	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("error opening file for S3 upload: %w", err)
	}
	defer file.Close()

	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("error getting file info: %w", err)
	}

	if fileInfo.Size() <= wavHeaderSize {
		return fmt.Errorf("file contains only WAV header, skipping upload: %s", filePath)
	}

	_, err = minioClient.FPutObject(ctx, s3BucketName, prefix+objectName, filePath, minio.PutObjectOptions{
		ContentType:  "audio/wav",
		UserMetadata: map[string]string{"x-amz-acl": "public-read"},
	})
	if err != nil {
		return fmt.Errorf("S3 upload failed: %w", err)
	}

	slog.Info("S3 upload completed", "filename", objectName, "size", fileInfo.Size())
	return nil
}
