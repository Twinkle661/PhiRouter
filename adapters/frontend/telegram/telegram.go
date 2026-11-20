// Package telegram implements the Bot API using net/http.
package telegram

import (
	"PhiRouter/internal/core"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const downloadLimit int64 = 20 * 1024 * 1024

type Options struct {
	Token           string
	UserID          int64
	Media           core.MediaStore
	VoiceMaxSeconds float64
	BaseURL         string
	Client          *http.Client
}
type Frontend struct {
	opt     Options
	client  *http.Client
	mu      sync.Mutex
	buttons map[string]answer
	replies map[int64]answer
}
type answer struct {
	reply   core.Reply
	chat    int64
	message int64
	title   string
	expires time.Time
}
type user struct {
	ID int64 `json:"id"`
}
type file struct {
	ID   string `json:"file_id"`
	Name string `json:"file_name"`
	MIME string `json:"mime_type"`
	Size int64  `json:"file_size"`
}
type message struct {
	ID   int64 `json:"message_id"`
	From user  `json:"from"`
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	Text      string   `json:"text"`
	Caption   string   `json:"caption"`
	Photo     []file   `json:"photo"`
	Document  *file    `json:"document"`
	Voice     *file    `json:"voice"`
	Audio     *file    `json:"audio"`
	Video     *file    `json:"video"`
	VideoNote *file    `json:"video_note"`
	Animation *file    `json:"animation"`
	Reply     *message `json:"reply_to_message"`
}
type update struct {
	ID       int64    `json:"update_id"`
	Message  *message `json:"message"`
	Callback *struct {
		ID      string   `json:"id"`
		From    user     `json:"from"`
		Data    string   `json:"data"`
		Message *message `json:"message"`
	} `json:"callback_query"`
}

func New(o Options) *Frontend {
	if o.BaseURL == "" {
		o.BaseURL = "https://api.telegram.org"
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: 65 * time.Second}
	}
	return &Frontend{opt: o, client: o.Client, buttons: map[string]answer{}, replies: map[int64]answer{}}
}
func (f *Frontend) api(ctx context.Context, method string, body any, result any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = f.request(ctx, method, "application/json", bytes.NewReader(raw), result)
		var limit *rateLimitError
		if !errors.As(err, &limit) || attempt >= 3 {
			return err
		}
		if err = waitRetry(ctx, limit.delay); err != nil {
			return err
		}
	}
}
func (f *Frontend) request(ctx context.Context, method, content string, body io.Reader, result any) error {
	req, err := http.NewRequestWithContext(ctx, "POST", f.opt.BaseURL+"/bot"+f.opt.Token+"/"+method, body)
	if err != nil {
		return errors.New("invalid Telegram endpoint")
	}
	req.Header.Set("Content-Type", content)
	res, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("Telegram %s transport failed", method)
	}
	defer res.Body.Close()
	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&envelope); err != nil {
		return fmt.Errorf("Telegram %s: invalid response", method)
	}
	if !envelope.OK {
		if envelope.ErrorCode == 429 {
			return &rateLimitError{time.Duration(max(envelope.Parameters.RetryAfter, 1)) * time.Second}
		}
		return fmt.Errorf("Telegram %s: %s", method, envelope.Description)
	}
	if result != nil {
		return json.Unmarshal(envelope.Result, result)
	}
	return nil
}
func (f *Frontend) Run(ctx context.Context, receive func(context.Context, core.Envelope) error) error {
	commands := []map[string]string{}
	for _, v := range [][2]string{{"retry", "Retry the last user message"}, {"restart", "Restart the backend"}, {"model", "Choose a model"}, {"new", "Start a new session"}, {"abort", "Stop generation"}, {"status", "Show backend status"}} {
		commands = append(commands, map[string]string{"command": v[0], "description": v[1]})
	}
	if err := f.api(ctx, "setMyCommands", map[string]any{"commands": commands}, nil); err != nil {
		slog.Error("command registration failed", "error", err)
	}
	var offset int64
	delay := time.Second
	for ctx.Err() == nil {
		var updates []update
		err := f.api(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 30, "allowed_updates": []string{"message", "callback_query"}}, &updates)
		if err != nil {
			slog.Error("Telegram polling failed", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			delay = min(delay*2, 30*time.Second)
			continue
		}
		delay = time.Second
		for _, u := range updates {
			offset = u.ID + 1
			if err = f.handle(ctx, u, receive); err != nil {
				chat := f.opt.UserID
				if u.Message != nil && u.Message.From.ID == f.opt.UserID {
					chat = u.Message.Chat.ID
				}
				if e := f.Send(ctx, strconv.FormatInt(chat, 10), core.Envelope{Parts: []core.Part{{Type: "text", Text: "[system] " + err.Error()}}}); e != nil {
					slog.Error("system message failed", "error", e, "message", err.Error())
				}
			}
		}
	}
	return nil
}
func (f *Frontend) handle(ctx context.Context, u update, receive func(context.Context, core.Envelope) error) error {
	if u.Callback != nil {
		q := u.Callback
		if q.From.ID != f.opt.UserID {
			slog.Debug("ignored unauthorized Telegram callback")
			return nil
		}
		if err := f.api(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": q.ID}, nil); err != nil {
			slog.Error("callback acknowledgement failed", "error", err)
		}
		f.mu.Lock()
		a, ok := f.buttons[q.Data]
		if ok && (q.Message == nil || q.Message.Chat.ID != a.chat || q.Message.ID != a.message || expired(a)) {
			ok = false
		}
		f.mu.Unlock()
		if !ok {
			return nil
		}
		if err := receive(ctx, core.Envelope{Source: "telegram:" + strconv.FormatInt(a.chat, 10), Reply: &a.reply}); err != nil {
			return err
		}
		f.clear(a.reply.ID, a.chat)
		return f.selected(ctx, a)
	}
	m := u.Message
	if m == nil {
		return nil
	}
	if m.From.ID != f.opt.UserID {
		slog.Debug("ignored unauthorized Telegram message")
		return nil
	}
	source := "telegram:" + strconv.FormatInt(m.Chat.ID, 10)
	if m.Reply != nil {
		f.mu.Lock()
		a, ok := f.replies[m.Reply.ID]
		f.mu.Unlock()
		if ok && a.chat == m.Chat.ID && !expired(a) {
			a.reply.Value = m.Text
			if err := receive(ctx, core.Envelope{Source: source, Reply: &a.reply}); err != nil {
				return err
			}
			f.clear(a.reply.ID, a.chat)
			return f.selected(ctx, a)
		}
	}
	e := core.Envelope{Source: source}
	if m.Text != "" {
		e.Parts = append(e.Parts, core.Part{Type: "text", Text: m.Text})
	}
	if m.Caption != "" {
		e.Parts = append(e.Parts, core.Part{Type: "text", Text: m.Caption})
	}
	var attachment *file
	kind := "file"
	if len(m.Photo) > 0 {
		attachment = &m.Photo[len(m.Photo)-1]
		kind = "image"
	}
	for _, v := range []*file{m.Document, m.Voice, m.Audio, m.Video, m.VideoNote, m.Animation} {
		if v != nil {
			attachment = v
			kind = "file"
			break
		}
	}
	if attachment != nil {
		if attachment.Size > downloadLimit {
			return errors.New("Telegram cannot download files larger than 20 MB. Please send a smaller file.")
		}
		p, err := f.download(ctx, *attachment)
		if err != nil {
			return err
		}
		if kind == "image" || strings.HasPrefix(p.MIME, "image/") {
			p.Type = "image"
		} else {
			p.Type = "file"
		}
		e.Parts = append(e.Parts, p)
	}
	if len(e.Parts) == 0 {
		return nil
	}
	return receive(ctx, e)
}
func (f *Frontend) download(ctx context.Context, a file) (core.Part, error) {
	var info struct {
		Path string `json:"file_path"`
		Size int64  `json:"file_size"`
	}
	if err := f.api(ctx, "getFile", map[string]string{"file_id": a.ID}, &info); err != nil {
		return core.Part{}, err
	}
	if info.Size > downloadLimit {
		return core.Part{}, errors.New("Telegram cannot download files larger than 20 MB")
	}
	if info.Path == "" {
		return core.Part{}, errors.New("Telegram returned no download path")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", f.opt.BaseURL+"/file/bot"+f.opt.Token+"/"+strings.TrimLeft(info.Path, "/"), nil)
	if err != nil {
		return core.Part{}, errors.New("invalid download URL")
	}
	res, err := f.client.Do(req)
	if err != nil {
		return core.Part{}, errors.New("Telegram file download failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return core.Part{}, fmt.Errorf("Telegram download status %d", res.StatusCode)
	}
	name := a.Name
	if name == "" {
		name = filepath.Base(info.Path)
	}
	return f.opt.Media.Save(ctx, name, a.MIME, res.Body, downloadLimit)
}
func (f *Frontend) Send(ctx context.Context, chat string, e core.Envelope) error {
	for _, p := range e.Parts {
		if p.Type == "text" {
			for _, text := range chunks(p.Text, 4096) {
				if strings.TrimSpace(text) == "" {
					continue
				}
				if err := f.api(ctx, "sendMessage", map[string]any{"chat_id": chat, "text": text}, nil); err != nil {
					return err
				}
			}
			continue
		}
		if err := f.sendFile(ctx, chat, p); err != nil {
			return err
		}
	}
	return nil
}
func (f *Frontend) Typing(ctx context.Context, chat string) error {
	return f.api(ctx, "sendChatAction", map[string]string{"chat_id": chat, "action": "typing"}, nil)
}
func (f *Frontend) sendFile(ctx context.Context, chat string, p core.Part) error {
	for attempt := 0; ; attempt++ {
		err := f.upload(ctx, chat, p)
		var limit *rateLimitError
		if !errors.As(err, &limit) || attempt >= 3 {
			return err
		}
		if err = waitRetry(ctx, limit.delay); err != nil {
			return err
		}
	}
}
func (f *Frontend) upload(ctx context.Context, chat string, p core.Part) error {
	q, err := f.opt.Media.Inspect(p.Path)
	if err != nil {
		return err
	}
	file, err := f.opt.Media.Open(q.Path)
	if err != nil {
		return err
	}
	defer file.Close()
	method, field := "sendDocument", "document"
	if f.voice(ctx, q) {
		method, field = "sendVoice", "voice"
	}
	pr, pw := io.Pipe()
	defer pr.Close()
	writer := multipart.NewWriter(pw)
	go func() {
		err := writer.WriteField("chat_id", chat)
		if err == nil && p.Caption != "" {
			err = writer.WriteField("caption", chunks(p.Caption, 1024)[0])
		}
		if err == nil {
			var part io.Writer
			part, err = writer.CreateFormFile(field, filepath.Base(q.Path))
			if err == nil {
				_, err = io.Copy(part, file)
			}
		}
		if err == nil {
			err = writer.Close()
		}
		_ = pw.CloseWithError(err)
	}()
	return f.request(ctx, method, writer.FormDataContentType(), pr, nil)
}
func (f *Frontend) voice(ctx context.Context, p core.Part) bool {
	ext := strings.ToLower(filepath.Ext(p.Path))
	if ext != ".ogg" && ext != ".opus" && ext != ".mp3" && ext != ".m4a" {
		return false
	}
	// Probe the already confined file through stdin, never a second pathname lookup.
	file, err := f.opt.Media.Open(p.Path)
	if err != nil {
		return false
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration:stream=codec_name,codec_type,duration", "-of", "json", "pipe:0")
	if fd, ok := file.(*os.File); ok {
		cmd.Args[len(cmd.Args)-1] = "/dev/fd/3"
		cmd.ExtraFiles = []*os.File{fd}
	} else {
		cmd.Stdin = file
	}
	data, err := cmd.Output()
	if err != nil {
		slog.Debug("audio probe failed", "error", err)
		return false
	}
	var probe struct {
		Format  struct{ Duration string }
		Streams []struct {
			CodecName string `json:"codec_name"`
			CodecType string `json:"codec_type"`
			Duration  string
		}
	}
	if json.Unmarshal(data, &probe) != nil {
		return false
	}
	duration, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	supported := false
	for _, s := range probe.Streams {
		if s.CodecType == "video" {
			return false
		}
		if d, _ := strconv.ParseFloat(s.Duration, 64); d > duration {
			duration = d
		}
		if s.CodecType == "audio" && (s.CodecName == "opus" || s.CodecName == "mp3" || s.CodecName == "aac") {
			supported = true
		}
	}
	return supported && duration > 0 && duration < f.opt.VoiceMaxSeconds
}
func (f *Frontend) Dialog(ctx context.Context, chat string, d core.Dialog) error {
	cid, err := strconv.ParseInt(chat, 10, 64)
	if err != nil {
		return err
	}
	text := d.Title
	if d.Message != "" {
		text += "\n" + d.Message
	}
	if d.Prefill != "" {
		text += "\n" + d.Prefill
	}
	if text == "" {
		text = "Please reply."
	}
	text = chunks(text, 4096)[0]
	body := map[string]any{"chat_id": chat, "text": text}
	expiry := time.Time{}
	if d.TimeoutMS > 0 {
		expiry = time.Now().Add(time.Duration(d.TimeoutMS) * time.Millisecond)
	}
	entries := map[string]answer{}
	if d.Method == "input" || d.Method == "editor" {
		body["reply_markup"] = map[string]any{"force_reply": true, "selective": true}
	} else {
		var rows [][]map[string]string
		options := d.Options
		if d.Method == "confirm" {
			options = []string{"Yes", "No"}
		} else {
			options = append(append([]string(nil), options...), "Cancel")
		}
		for i, label := range options {
			buf := make([]byte, 12)
			if _, err = rand.Read(buf); err != nil {
				return err
			}
			token := hex.EncodeToString(buf)
			reply := core.Reply{ID: d.ID, Value: label, Confirmed: label == "Yes", Cancelled: d.Method != "confirm" && i == len(options)-1}
			entries[token] = answer{reply: reply, chat: cid, title: d.Title, expires: expiry}
			rows = append(rows, []map[string]string{{"text": label, "callback_data": token}})
		}
		// Telegram limits keyboards to 100 buttons; split large model menus into pages of messages.
		if len(rows) > 90 {
			for start := 0; start < len(d.Options); start += 89 {
				part := d
				part.Options = d.Options[start:min(start+89, len(d.Options))]
				if err = f.Dialog(ctx, chat, part); err != nil {
					return err
				}
			}
			return nil
		}
		body["reply_markup"] = map[string]any{"inline_keyboard": rows}
	}
	// Hold the lock until the Telegram message ID is registered, so fast replies cannot race registration.
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, a := range f.buttons {
		if expired(a) {
			delete(f.buttons, k)
		}
	}
	for k, a := range f.replies {
		if expired(a) {
			delete(f.replies, k)
		}
	}
	var sent message
	if err = f.api(ctx, "sendMessage", body, &sent); err != nil {
		return err
	}
	for k, a := range entries {
		a.message = sent.ID
		f.buttons[k] = a
	}
	if len(entries) == 0 {
		f.replies[sent.ID] = answer{reply: core.Reply{ID: d.ID}, chat: cid, message: sent.ID, title: d.Title, expires: expiry}
	}
	return nil
}
func expired(a answer) bool { return !a.expires.IsZero() && time.Now().After(a.expires) }
func (f *Frontend) clear(id string, chat int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, a := range f.buttons {
		if a.reply.ID == id && a.chat == chat {
			delete(f.buttons, k)
		}
	}
	for k, a := range f.replies {
		if a.reply.ID == id && a.chat == chat {
			delete(f.replies, k)
		}
	}
}
func (f *Frontend) selected(ctx context.Context, a answer) error {
	value := a.reply.Value
	if a.reply.Cancelled {
		value = "Cancelled"
	}
	return f.api(ctx, "editMessageText", map[string]any{"chat_id": a.chat, "message_id": a.message, "text": chunks(a.title+"\nSelected: "+value, 4096)[0], "reply_markup": map[string]any{"inline_keyboard": [][]any{}}}, nil)
}
func chunks(s string, limit int) []string {
	var out []string
	start, units := 0, 0
	for i, r := range s {
		u := 1
		if r > 0xffff {
			u = 2
		}
		if units+u > limit {
			out = append(out, s[start:i])
			start = i
			units = 0
		}
		units += u
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

type rateLimitError struct{ delay time.Duration }

func (e *rateLimitError) Error() string { return "Telegram rate limit exceeded" }
func waitRetry(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
