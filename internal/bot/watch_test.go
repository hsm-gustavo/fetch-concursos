package bot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/redis/go-redis/v9"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestWatchBaselineAndMonitorDedup(t *testing.T) {
	ctx := context.Background()
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer redisClient.Close()

	fixture, err := os.ReadFile("testdata/cargos.html")
	if err != nil {
		t.Fatal(err)
	}
	vagas, err := os.ReadFile("testdata/vagas.html")
	if err != nil {
		t.Fatal(err)
	}
	cargosHTML := string(fixture)
	var pciRequests atomic.Int32
	client := &http.Client{Timeout: time.Second, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		pciRequests.Add(1)
		body := cargosHTML
		if strings.Contains(request.URL.Path, "/vagas/") {
			body = string(vagas)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: request}, nil
	})}
	var sent atomic.Int32
	telegramServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/getMe") {
			_, _ = io.WriteString(writer, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test"}}`)
			return
		}
		if strings.HasSuffix(request.URL.Path, "/sendMessage") {
			sent.Add(1)
		}
		_, _ = io.WriteString(writer, `{"ok":true,"result":{"message_id":1,"chat":{"id":123,"type":"private"}}}`)
	}))
	defer telegramServer.Close()
	telegram, err := tgbotapi.NewBotAPIWithClient("test", telegramServer.URL+"/bot%s/%s", telegramServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{telegram: telegram, redis: redisClient, httpClient: client, logger: slog.Default(), allowedUserID: 123}
	option, _ := optionByID("analista-sistemas")
	if _, err := service.fetchConcursos(ctx, option.URL); err != nil {
		t.Fatal(err)
	}
	if _, err := service.fetchConcursos(ctx, option.URL); err != nil {
		t.Fatal(err)
	}
	if pciRequests.Load() != 2 {
		t.Fatalf("cache made %d PCI requests, want 2", pciRequests.Load())
	}
	if err := service.setWatch(ctx, option, "SP", true); err != nil {
		t.Fatal(err)
	}
	if !service.isWatching(ctx, option.ID, "SP") {
		t.Fatal("watch not persisted")
	}
	if count, err := redisClient.SCard(ctx, seenKey(watchID(option.ID, "SP"))).Result(); err != nil || count != 1 {
		t.Fatalf("baseline count = %d, %v", count, err)
	}
	message := &tgbotapi.Message{MessageID: 1, Chat: &tgbotapi.Chat{ID: 123}}
	service.handleWatchCallback(ctx, message, option, []string{"watchremove", option.ID, "SP"})
	service.handleWatchCallback(ctx, message, option, []string{"watchremove", option.ID, "SP"})
	if service.isWatching(ctx, option.ID, "SP") {
		t.Fatal("repeated removal reactivated watch")
	}
	service.handleWatchCallback(ctx, message, option, []string{"watchadd", option.ID, "SP"})
	service.handleWatchCallback(ctx, message, option, []string{"watchadd", option.ID, "SP"})
	if !service.isWatching(ctx, option.ID, "SP") {
		t.Fatal("repeated activation removed watch")
	}
	service.handleWatchCallback(ctx, message, option, []string{"watch", option.ID, "SP"})
	if !service.isWatching(ctx, option.ID, "SP") {
		t.Fatal("legacy callback changed watch")
	}
	service.checkWatches(ctx)
	if sent.Load() != 0 {
		t.Fatal("existing publication was notified")
	}

	newEntry := `<ul class="link-d"><li><a href="https://www.pciconcursos.com.br/noticias/nova-publicacao">Novo Órgão - SP</a></li></ul><ul class="link-i"><li><a href="https://www.pciconcursos.com.br/vagas/analista-de-sistemas">Analista de Sistemas</a></li></ul>`
	cargosHTML = strings.Replace(cargosHTML, "</div></body></html>", newEntry+"</div></body></html>", 1)
	service.checkWatches(ctx)
	service.checkWatches(ctx)
	if sent.Load() != 1 {
		t.Fatalf("sent %d messages, want 1", sent.Load())
	}
	if pciRequests.Load() < 6 {
		t.Fatalf("monitor did not refetch PCI: %d requests", pciRequests.Load())
	}
}

func TestEmptyListsHaveArrayKeyboard(t *testing.T) {
	redisServer := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer client.Close()
	service := &Service{redis: client, logger: slog.Default()}
	for _, list := range []func(context.Context) (string, tgbotapi.InlineKeyboardMarkup, error){service.watchList, service.favoriteList} {
		_, keyboard, err := list(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(keyboard)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != `{"inline_keyboard":[]}` {
			t.Fatalf("empty keyboard = %s", data)
		}
	}
}

func TestEditHTMLIgnoresUnchangedMessage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/getMe") {
			_, _ = io.WriteString(writer, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test"}}`)
			return
		}
		_, _ = io.WriteString(writer, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified: specified new message content and reply markup are exactly the same as a current content and reply markup of the message"}`)
	}))
	defer server.Close()
	telegram, err := tgbotapi.NewBotAPIWithClient("test", server.URL+"/bot%s/%s", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{telegram: telegram}
	if err := service.editHTML(123, 1, "Mesmo texto", emptyInlineKeyboard()); err != nil {
		t.Fatalf("unchanged edit returned %v", err)
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	redisServer := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: redisServer.Addr()})
	defer redisClient.Close()
	telegramServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(request.URL.Path, "/getMe"):
			_, _ = io.WriteString(writer, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"Test"}}`)
		case strings.HasSuffix(request.URL.Path, "/getUpdates"):
			time.Sleep(20 * time.Millisecond)
			_, _ = io.WriteString(writer, `{"ok":true,"result":[]}`)
		default:
			_, _ = io.WriteString(writer, `{"ok":true,"result":true}`)
		}
	}))
	defer telegramServer.Close()
	telegram, err := tgbotapi.NewBotAPIWithClient("test", telegramServer.URL+"/bot%s/%s", telegramServer.Client())
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{telegram: telegram, redis: redisClient, logger: slog.Default(), healthAddr: "127.0.0.1:0", checkInterval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestHealthHandler(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	handler := healthHandler(ctx)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("healthy status = %d", response.Code)
	}
	cancel()
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("shutdown status = %d", response.Code)
	}
}
