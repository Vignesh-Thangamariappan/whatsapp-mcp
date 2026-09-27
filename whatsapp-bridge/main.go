package main

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"

	"bytes"

	"github.com/purpshell/meowcaller"

	"github.com/polymorfa/hypermeow"
	waProto "github.com/polymorfa/hypermeow/binary/proto"
	"github.com/polymorfa/hypermeow/store/sqlstore"
	"github.com/polymorfa/hypermeow/types"
	"github.com/polymorfa/hypermeow/types/events"
	waLog "github.com/polymorfa/hypermeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0700); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", "file:store/messages.db?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages 
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// For now, we're ignoring non-text messages
	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, recipient string, message string, mediaPath string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waProto.Message{}

	// Check if we have media to send
	if mediaPath != "" {
		// media_path accepts any absolute path by design (that's the whole point of
		// send_file/send_audio_message), but that means an LLM tricked by a prompt
		// injection inside a WhatsApp message could point it at the bridge's own
		// session DB and exfiltrate it via WhatsApp to take over the account. Block
		// just the bridge's own top-level store files (whatsapp.db, messages.db,
		// api_token.txt); per-chat subdirectories still work, so re-sending media
		// you already downloaded from a chat is unaffected.
		if blocked, err := isBlockedStorePath(mediaPath); err != nil {
			return false, fmt.Sprintf("Error validating media path: %v", err)
		} else if blocked {
			return false, "Refusing to send a file from the bridge's own store directory (session keys / API token live there)"
		}

		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types (for any other file type)
		default:
			mediaType = whatsmeow.MediaDocument
			mimeType = "application/octet-stream"
		}

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		// Don't log the full upload response - it contains MediaKey/FileEncSHA256,
		// the encryption material needed to decrypt the media from WhatsApp's CDN.
		fmt.Printf("Media uploaded (%d bytes)\n", len(mediaData))

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			msg.DocumentMessage = &waProto.DocumentMessage{
				Title:         proto.String(mediaPath[strings.LastIndex(mediaPath, "/")+1:]),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	_, err = client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}

// PlaceCallRequest represents the request body for the place call API
type PlaceCallRequest struct {
	Recipient string `json:"recipient"`
}

// PlaceCallResponse represents the response for the place call API
type PlaceCallResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	CallID  string `json:"call_id,omitempty"`
}

// HangupCallRequest represents the request body for the hangup call API
type HangupCallRequest struct {
	CallID string `json:"call_id"`
}

// ConverseRequest represents the request body for the call converse API
type ConverseRequest struct {
	CallID  string `json:"call_id"`
	Message string `json:"message"`
}

// ConverseResponse represents the response for the call converse API
type ConverseResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Reply   string `json:"reply,omitempty"`
}

// trackedCall pairs a placed call with a channel that closes once the peer answers
// and media starts flowing (mirrors Call.OnReady, which only supports one callback -
// CallManager owns that slot so callConverse doesn't have to fight over it turn by
// turn).
type trackedCall struct {
	call  *meowcaller.Call
	ready chan struct{}
}

// CallManager tracks calls this bridge has placed, keyed by meowcaller's call ID,
// so later /api/call/hangup and /api/call/converse requests can find the live
// *meowcaller.Call. Entries remove themselves once the call ends (answered,
// rejected, or timed out).
type CallManager struct {
	mu    sync.Mutex
	calls map[string]*trackedCall
}

// NewCallManager returns an empty CallManager.
func NewCallManager() *CallManager {
	return &CallManager{calls: make(map[string]*trackedCall)}
}

// Track registers a newly placed call and removes it once it ends.
func (m *CallManager) Track(call *meowcaller.Call) {
	entry := &trackedCall{call: call, ready: make(chan struct{})}

	m.mu.Lock()
	m.calls[call.ID()] = entry
	m.mu.Unlock()

	call.OnReady(func() {
		close(entry.ready)
	})

	call.OnEnd(func(reason string) {
		m.mu.Lock()
		delete(m.calls, call.ID())
		m.mu.Unlock()
		fmt.Printf("Call %s ended: %s\n", call.ID(), reason)
	})
}

// Get looks up a tracked call by ID, along with its ready channel (closed once the
// peer has answered and media is flowing).
func (m *CallManager) Get(callID string) (call *meowcaller.Call, ready chan struct{}, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.calls[callID]
	if !ok {
		return nil, nil, false
	}
	return entry.call, entry.ready, true
}

// isAllowedCallTarget reports whether recipient may be called. If
// WHATSAPP_CALL_ALLOWLIST is unset, every recipient is allowed (matching
// send_message's existing trust model). If set, it's a comma-separated list of
// phone numbers/JIDs and recipient must match one of them (bare-number comparison,
// so "919952743202" matches "919952743202@s.whatsapp.net"). This exists because
// call_converse lets a call actually talk, which turns the pre-existing "any
// WhatsApp contact can prompt-inject the LLM" gap (ROADMAP.md, P3) from "send an
// unwanted message" into "hold an AI-voiced conversation with an arbitrary number" -
// worth a cheap extra gate specifically for calling.
func isAllowedCallTarget(recipient string) bool {
	allowlist := os.Getenv("WHATSAPP_CALL_ALLOWLIST")
	if allowlist == "" {
		return true
	}
	bareRecipient := strings.SplitN(recipient, "@", 2)[0]
	for _, entry := range strings.Split(allowlist, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if entry == recipient || strings.SplitN(entry, "@", 2)[0] == bareRecipient {
			return true
		}
	}
	return false
}

// placeCall places an outbound 1:1 voice call. It only places the call (signaling)
// itself; no audio is attached until a separate call_converse turn plays and
// listens on it. Placing a call should never auto-answer or auto-record an
// incoming one - that's a different, deliberately unimplemented code path.
func placeCall(callClient *meowcaller.Client, calls *CallManager, recipient string) (bool, string, string) {
	if callClient == nil {
		return false, "Calling is not initialized", ""
	}
	if recipient == "" {
		return false, "Recipient must be provided", ""
	}
	if !isAllowedCallTarget(recipient) {
		return false, "Recipient is not on WHATSAPP_CALL_ALLOWLIST", ""
	}

	call, err := callClient.Call(context.Background(), recipient)
	if err != nil {
		return false, fmt.Sprintf("Error placing call: %v", err), ""
	}

	calls.Track(call)
	fmt.Printf("Call placed to %s (call_id=%s)\n", recipient, call.ID())
	return true, fmt.Sprintf("Call placed to %s", recipient), call.ID()
}

// hangupCall ends a call previously placed via placeCall, identified by its call ID.
func hangupCall(calls *CallManager, callID string) (bool, string) {
	if callID == "" {
		return false, "call_id must be provided"
	}

	call, _, ok := calls.Get(callID)
	if !ok {
		return false, "Unknown or already-ended call ID"
	}

	if err := call.Hangup(); err != nil {
		return false, fmt.Sprintf("Error hanging up call: %v", err)
	}

	return true, "Call ended"
}

// --- Speech in/out for call_converse, built on the local voice-mode stack already
// running on this machine (Kokoro TTS, whisper.cpp STT) - no third-party network
// egress, consistent with today's telemetry review.

// synthesizeSpeech renders text to a 16-bit PCM WAV file via the local Kokoro TTS
// server and returns its path. The caller is responsible for removing it.
func synthesizeSpeech(text string) (string, error) {
	ttsURL := os.Getenv("WHATSAPP_CALL_TTS_URL")
	if ttsURL == "" {
		ttsURL = "http://127.0.0.1:8880/v1/audio/speech"
	}
	voice := os.Getenv("WHATSAPP_CALL_TTS_VOICE")
	if voice == "" {
		voice = "af_heart"
	}

	reqBody, err := json.Marshal(map[string]any{
		"model":           "kokoro",
		"input":           text,
		"voice":           voice,
		"response_format": "wav",
		"stream":          false,
	})
	if err != nil {
		return "", fmt.Errorf("encode TTS request: %w", err)
	}

	resp, err := http.Post(ttsURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("TTS request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("TTS server returned %d: %s", resp.StatusCode, string(body))
	}

	outFile, err := os.CreateTemp("", "whatsapp-call-tts-*.wav")
	if err != nil {
		return "", fmt.Errorf("create TTS temp file: %w", err)
	}
	defer outFile.Close()
	if _, err := io.Copy(outFile, resp.Body); err != nil {
		os.Remove(outFile.Name())
		return "", fmt.Errorf("save TTS audio: %w", err)
	}

	return outFile.Name(), nil
}

// transcribeAudio sends a WAV file to the local whisper.cpp server and returns its
// transcript.
func transcribeAudio(wavPath string) (string, error) {
	sttURL := os.Getenv("WHATSAPP_CALL_STT_URL")
	if sttURL == "" {
		sttURL = "http://127.0.0.1:2022/v1/audio/transcriptions"
	}

	f, err := os.Open(wavPath)
	if err != nil {
		return "", fmt.Errorf("open recorded audio: %w", err)
	}
	defer f.Close()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filepath.Base(wavPath))
	if err != nil {
		return "", fmt.Errorf("build STT request: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("attach recorded audio: %w", err)
	}
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("finalize STT request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, sttURL, &body)
	if err != nil {
		return "", fmt.Errorf("build STT request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("STT request: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("parse STT response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Error != "" {
		return "", fmt.Errorf("STT server error: %s", result.Error)
	}

	return strings.TrimSpace(result.Text), nil
}

// vadEnergyThreshold is the RMS level (on [-1,1) float32 samples) above which a
// frame counts as speech rather than silence/background. Not empirically tuned yet
// against a real call - the first live test is exactly what tunes it.
const vadEnergyThreshold = 0.02

// silenceFramesToEndTurn is how many consecutive silent 60ms frames (after speech
// has started) count as "the peer stopped talking". 14 frames is ~840ms.
const silenceFramesToEndTurn = 14

// maxTurnFrames caps a single listen turn regardless of VAD, so a stuck/silent line
// can't hang call_converse forever. 500 frames is ~30s.
const maxTurnFrames = 500

// rmsEnergy returns the root-mean-square amplitude of a mono float32 frame.
func rmsEnergy(frame []float32) float64 {
	if len(frame) == 0 {
		return 0
	}
	var sum float64
	for _, s := range frame {
		sum += float64(s) * float64(s)
	}
	return math.Sqrt(sum / float64(len(frame)))
}

// endpointRecorder wraps a meowcaller.AudioSink (normally a WAVRecorder) with a
// simple energy-based endpointer: it signals done once the peer has spoken and then
// gone quiet for silenceFramesToEndTurn frames, or once maxTurnFrames is reached
// regardless. This is a deliberately simple VAD, not full noise-robust endpointing.
type endpointRecorder struct {
	mu          sync.Mutex
	inner       meowcaller.AudioSink
	started     bool
	silenceRun  int
	totalFrames int
	done        chan struct{}
	closed      bool
}

func newEndpointRecorder(inner meowcaller.AudioSink) *endpointRecorder {
	return &endpointRecorder{inner: inner, done: make(chan struct{})}
}

func (l *endpointRecorder) WriteFrame(frame []float32) error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}

	energy := rmsEnergy(frame)
	if energy > vadEnergyThreshold {
		l.started = true
		l.silenceRun = 0
	} else if l.started {
		l.silenceRun++
	}
	l.totalFrames++
	shouldFinish := (l.started && l.silenceRun >= silenceFramesToEndTurn) || l.totalFrames >= maxTurnFrames
	l.mu.Unlock()

	if err := l.inner.WriteFrame(frame); err != nil {
		return err
	}

	if shouldFinish {
		l.signalDone()
	}
	return nil
}

func (l *endpointRecorder) signalDone() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.done)
	}
}

// Close satisfies AudioSink; it does not close the underlying recorder (the caller
// does that explicitly after the turn ends, so the WAV header gets finalized once).
func (l *endpointRecorder) Close() error { return nil }

// wait blocks until end-of-turn (VAD-detected silence, the frame cap, or ctx being
// done), whichever comes first.
func (l *endpointRecorder) wait(ctx context.Context, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-l.done:
	case <-timer.C:
		l.signalDone()
	case <-ctx.Done():
		l.signalDone()
	}
}

// converseOnCall plays message to an already-placed call, then listens for and
// transcribes the peer's spoken reply. It waits for the call to actually be
// answered (media flowing) before playing anything, so a turn placed the instant
// after place_call returns doesn't talk into a still-ringing line.
func converseOnCall(calls *CallManager, callID, message string) (success bool, status string, reply string) {
	if callID == "" {
		return false, "call_id must be provided", ""
	}
	if message == "" {
		return false, "message must be provided", ""
	}

	call, ready, ok := calls.Get(callID)
	if !ok {
		return false, "Unknown or already-ended call ID", ""
	}

	select {
	case <-ready:
	case <-time.After(45 * time.Second):
		return false, "Timed out waiting for the call to be answered", ""
	}

	wavPath, err := synthesizeSpeech(message)
	if err != nil {
		return false, fmt.Sprintf("Speech synthesis failed: %v", err), ""
	}
	defer os.Remove(wavPath)

	src, err := meowcaller.WAVFile(wavPath)
	if err != nil {
		return false, fmt.Sprintf("Failed to load synthesized audio: %v", err), ""
	}

	recordPath := filepath.Join(os.TempDir(), "whatsapp-call-"+callID+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".wav")
	rawRecorder, err := meowcaller.WAVRecorder(recordPath)
	if err != nil {
		return false, fmt.Sprintf("Failed to open recorder: %v", err), ""
	}
	listener := newEndpointRecorder(rawRecorder)
	call.Receive(listener)

	player := call.Play(src)
	playFinished := make(chan struct{})
	player.OnFinish(func() { close(playFinished) })

	// Give the peer a moment to hear the message before we start counting silence
	// against them - otherwise the gap between "message finishes" and "person starts
	// talking" can itself trip the end-of-turn silence threshold.
	select {
	case <-playFinished:
	case <-time.After(30 * time.Second):
	}

	listener.wait(context.Background(), 30*time.Second)
	rawRecorder.Close()
	defer os.Remove(recordPath)

	transcript, err := transcribeAudio(recordPath)
	if err != nil {
		return false, fmt.Sprintf("Transcription failed: %v", err), ""
	}

	return true, "ok", transcript
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Save message to database
	chatJID := msg.Info.Chat.String()
	sender := msg.Info.Sender.User

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, sender, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, sender, content)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file
	chatDir := fmt.Sprintf("store/%s", strings.ReplaceAll(chatJID, ":", "_"))
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Sanitize the filename before using it in any filesystem path. The filename for
	// document messages comes from the sender (doc.GetFileName()) and is otherwise
	// attacker-controlled - without this, a crafted filename like "../../../.ssh/authorized_keys"
	// would let anyone who messages the account write files outside chatDir (CWE-22).
	// filepath.Base strips any path separators, but ".." itself contains none and
	// still means "parent directory", so it has to be rejected explicitly.
	filename = filepath.Base(filepath.Clean(filename))
	if filename == "" || filename == "." || filename == ".." || filename == "/" || filename == string(filepath.Separator) {
		filename = "file_" + time.Now().Format("20060102_150405")
	}

	// Create directory for the chat if it doesn't exist
	if err := os.MkdirAll(chatDir, 0700); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Generate a local path for the file
	localPath = fmt.Sprintf("%s/%s", chatDir, filename)

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Defense in depth: confirm the resolved path is still inside chatDir. (Given the
	// sanitization above, filename can no longer contain a separator or be "..", so
	// this should always hold - it's a backstop against that invariant breaking later,
	// not the primary protection.)
	absChatDir, err := filepath.Abs(chatDir)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to resolve chat directory: %v", err)
	}
	if !strings.HasPrefix(absPath, absChatDir+string(filepath.Separator)) {
		return false, "", "", "", fmt.Errorf("invalid media filename")
	}

	// Check if file already exists
	if _, err := os.Stat(localPath); err == nil {
		// File exists, return it
		return true, mediaType, filename, absPath, nil
	}

	// If we don't have all the media info we need, we can't download
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	fmt.Printf("Attempting to download media for message %s in chat %s...\n", messageID, chatJID)

	// Extract direct path from URL
	directPath := extractDirectPathFromURL(url)

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	// Download the media using whatsmeow client
	mediaData, err := client.Download(context.Background(), downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	// Save the downloaded media to file
	if err := os.WriteFile(localPath, mediaData, 0600); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Successfully downloaded %s media to %s (%d bytes)\n", mediaType, absPath, len(mediaData))
	return true, mediaType, filename, absPath, nil
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	// Find the path part after the domain
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	pathPart := parts[1]

	// Remove query parameters
	pathPart = strings.SplitN(pathPart, "?", 2)[0]

	// Create proper direct path format
	return "/" + pathPart
}

// isBlockedStorePath reports whether path resolves to one of the bridge's own
// top-level files under store/ (the session/message SQLite databases and the API
// token) rather than a per-chat subdirectory. Resolves symlinks on both sides so a
// symlinked media_path (or a symlinked store/) can't be used to route around it.
func isBlockedStorePath(path string) (bool, error) {
	storeAbs, err := filepath.Abs("store")
	if err != nil {
		return false, err
	}
	storeReal, err := filepath.EvalSymlinks(storeAbs)
	if err != nil {
		// store/ not resolvable (e.g. doesn't exist yet) - fall back to the
		// unresolved absolute path rather than skipping the check.
		storeReal = storeAbs
	}

	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	pathReal, err := filepath.EvalSymlinks(pathAbs)
	if err != nil {
		pathReal = pathAbs
	}

	if pathReal == storeReal {
		return true, nil
	}
	if !strings.HasPrefix(pathReal, storeReal+string(filepath.Separator)) {
		return false, nil
	}

	rel, err := filepath.Rel(storeReal, pathReal)
	if err != nil {
		return true, nil // fail safe: can't establish the relation, block it
	}
	// Only top-level children of store/ are sensitive (whatsapp.db, messages.db,
	// api_token.txt, plus SQLite -wal/-shm sidecars). Per-chat media lives one
	// level deeper (store/<chat>/<file>) and is fine to re-send.
	return !strings.Contains(rel, string(filepath.Separator)), nil
}

// loadOrCreateAPIToken returns the shared secret required to call the REST API.
// It honors WHATSAPP_BRIDGE_TOKEN if set, otherwise persists a randomly generated
// token to store/api_token.txt so it survives restarts. Without this, the API had
// no authentication at all, so anything that could reach the port (a LAN peer, or a
// browser tab via CSRF against localhost) could send messages or exfiltrate media
// as the logged-in account.
func loadOrCreateAPIToken() (string, error) {
	if token := os.Getenv("WHATSAPP_BRIDGE_TOKEN"); token != "" {
		return token, nil
	}

	tokenPath := "store/api_token.txt"
	if data, err := os.ReadFile(tokenPath); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	}

	if err := os.MkdirAll("store", 0700); err != nil {
		return "", fmt.Errorf("failed to create store directory: %v", err)
	}

	raw := make([]byte, 32)
	if _, err := cryptorand.Read(raw); err != nil {
		return "", fmt.Errorf("failed to generate API token: %v", err)
	}
	token := hex.EncodeToString(raw)

	if err := os.WriteFile(tokenPath, []byte(token), 0600); err != nil {
		return "", fmt.Errorf("failed to persist API token: %v", err)
	}

	return token, nil
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, callClient *meowcaller.Client, calls *CallManager, port int) {
	token, err := loadOrCreateAPIToken()
	if err != nil {
		fmt.Printf("Failed to set up REST API authentication: %v\n", err)
		return
	}

	// requireAuth rejects any request that doesn't present the shared-secret token
	// as "Authorization: Bearer <token>". Uses a constant-time comparison to avoid
	// leaking the token via response-timing side channels.
	requireAuth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			authHeader := r.Header.Get("Authorization")
			presented := strings.TrimPrefix(authHeader, prefix)
			if !strings.HasPrefix(authHeader, prefix) ||
				subtle.ConstantTimeCompare([]byte(presented), []byte(token)) != 1 {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}

	// Handler for sending messages
	http.HandleFunc("/api/send", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		fmt.Printf("Received request to send message to %s (media: %v)\n", req.Recipient, req.MediaPath != "")

		// Send the message
		success, message := sendWhatsAppMessage(client, req.Recipient, req.Message, req.MediaPath)
		fmt.Println("Message sent", success)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	}))

	// Handler for downloading media
	http.HandleFunc("/api/download", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	}))

	// Handler for placing an outbound call. Same auth gating as everything else on
	// this server - a call is a real, immediate effect on the recipient's phone, so
	// it gets no more (and no less) trust than /api/send.
	http.HandleFunc("/api/call", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req PlaceCallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		fmt.Printf("Received request to place a call to %s\n", req.Recipient)

		success, message, callID := placeCall(callClient, calls, req.Recipient)
		fmt.Println("Call placed", success)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		json.NewEncoder(w).Encode(PlaceCallResponse{
			Success: success,
			Message: message,
			CallID:  callID,
		})
	}))

	// Handler for hanging up a call this bridge placed.
	http.HandleFunc("/api/call/hangup", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req HangupCallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.CallID == "" {
			http.Error(w, "call_id is required", http.StatusBadRequest)
			return
		}

		success, message := hangupCall(calls, req.CallID)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	}))

	// Handler for speaking a message on an already-placed call and returning the
	// peer's transcribed spoken reply. This is a slow endpoint by nature (it waits
	// for the call to be answered, plays audio, then listens for a real person to
	// finish talking), so give it generous timeouts on the client side.
	http.HandleFunc("/api/call/converse", requireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req ConverseRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		if req.CallID == "" || req.Message == "" {
			http.Error(w, "call_id and message are required", http.StatusBadRequest)
			return
		}

		fmt.Printf("Speaking on call %s: %q\n", req.CallID, req.Message)

		success, message, reply := converseOnCall(calls, req.CallID, req.Message)
		fmt.Printf("Converse turn on call %s: success=%v reply=%q\n", req.CallID, success, reply)

		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		json.NewEncoder(w).Encode(ConverseResponse{
			Success: success,
			Message: message,
			Reply:   reply,
		})
	}))

	// Start the server. Binds to 127.0.0.1 by default - the API previously bound to
	// all interfaces (":8080"), so anyone on the same LAN/Wi-Fi could reach it.
	// Override with WHATSAPP_BRIDGE_HOST if you deliberately want to expose it
	// (e.g. behind an SSH tunnel or reverse proxy you control).
	host := os.Getenv("WHATSAPP_BRIDGE_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	serverAddr := fmt.Sprintf("%s:%d", host, port)
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)
	// Don't print the token itself - stdout here is exactly what people redirect to
	// whatsapp.log (see the README's own troubleshooting section), which would leak
	// it into a file that then needs the same protection as the token file itself.
	// The Python MCP server reads store/api_token.txt automatically; only cat it
	// yourself if you need to copy it to WHATSAPP_BRIDGE_TOKEN on another machine.
	fmt.Println("REST API auth token loaded (see store/api_token.txt; set WHATSAPP_BRIDGE_TOKEN to override)")

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := http.ListenAndServe(serverAddr, nil); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0700); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Wrap the client with meowcaller's managed calling API. This must happen before
	// client.Connect() below so the call-signaling event handlers are installed
	// before the receive loop starts. No incoming-call handling is wired up here:
	// this bridge only places outbound calls, it never auto-answers or auto-records
	// an inbound one.
	callClient := meowcaller.NewClient(client)
	callManager := NewCallManager()

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
			} else if evt.Event == "success" {
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Start REST API server
	startRESTServer(client, messageStore, callClient, callManager, 8080)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Just use contact info (full name)
		contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
		if err == nil && contact.FullName != "" {
			name = contact.FullName
		} else if sender != "" {
			// Fallback to sender
			name = sender
		} else {
			// Last fallback to JID
			name = jid.User
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content
				var content string
				if msg.Message.Message != nil {
					if conv := msg.Message.Message.GetConversation(); conv != "" {
						content = conv
					} else if ext := msg.Message.Message.GetExtendedTextMessage(); ext != nil {
						content = ext.GetText()
					}
				}

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Log presence/shape only - not the actual message content, which
				// would otherwise dump every synced message's plaintext to stdout
				// (and, per the README's own troubleshooting section, to whatsapp.log).
				logger.Infof("Message length: %d, Media Type: %v", len(content), mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						sender = *msg.Message.Key.Participant
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage - metadata only, not content (see note above)
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s]",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s (%d chars)",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, len(content))
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// Found OpusHead, extract sample rate and pre-skip
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8 // Skip "OpusHead" marker
				// PreSkip is 2 bytes at offset 10
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					fmt.Printf("Found OpusHead: sampleRate=%d, preSkip=%d\n", sampleRate, preSkip)
				}
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rand.Seed(int64(duration))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rand.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
