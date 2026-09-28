package bot

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/redis/go-redis/v9"
	"golang.org/x/net/html"
)

const (
	parseModeHTML     = "HTML"
	maxMessageLen     = 3800
	pageSize          = 10
	statesPageSize    = 8
	statesAPIURL      = "https://servicodados.ibge.gov.br/api/v1/localidades/estados?orderBy=nome"
	statesCacheKey    = "fetch-concursos:estados:v1"
	statesCacheTTL    = 30 * 24 * time.Hour
	concursosCacheTTL = 5 * time.Minute
	defaultRedisURL   = "redis://localhost:6379/0"
)

type Config struct {
	TelegramToken string
	RedisURL      string
	AllowedUserID int64
	Debug         bool
	HealthAddr    string
	CheckInterval time.Duration
	HistoryPath   string
}

type Service struct {
	telegram      *tgbotapi.BotAPI
	redis         *redis.Client
	httpClient    *http.Client
	allowedUserID int64
	logger        *slog.Logger
	healthAddr    string
	checkInterval time.Duration
	watchMu       sync.Mutex
	history       *sql.DB
}

type Concurso struct {
	ID           string
	Orgao        string
	Cargo        string
	UF           string
	NoticiaURL   string
	CargoURL     string
	Vagas        string
	Remuneracao  string
	Escolaridade string
	Prazo        string
}

type CargoOption struct {
	ID   string
	Nome string
	URL  string
}

type Estado struct {
	Sigla string `json:"sigla"`
	Nome  string `json:"nome"`
}

var cargoOptions = []CargoOption{
	{ID: "analista-sistemas", Nome: "Analista de Sistemas", URL: "https://www.pciconcursos.com.br/cargos/analista-de-sistemas"},
	{ID: "analista-tecnico", Nome: "Analista Técnico - Informática", URL: "https://www.pciconcursos.com.br/cargos/analista-tecnico-informatica"},
	{ID: "tecnico-em", Nome: "Técnico em Informática", URL: "https://www.pciconcursos.com.br/cargos/tecnico-em-informatica"},
	{ID: "tecnico-de", Nome: "Técnico de Informática", URL: "https://www.pciconcursos.com.br/cargos/tecnico-de-informatica"},
}

func New(config Config, logger *slog.Logger) (*Service, error) {
	if config.TelegramToken == "" {
		return nil, fmt.Errorf("TELEGRAM_APITOKEN is required")
	}
	if config.AllowedUserID == 0 {
		return nil, fmt.Errorf("allowed Telegram user ID is required")
	}
	if logger == nil {
		logger = slog.Default()
	}

	telegram, err := tgbotapi.NewBotAPIWithClient(config.TelegramToken, tgbotapi.APIEndpoint, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("create Telegram client: %w", err)
	}
	telegram.Debug = config.Debug

	redisURL := config.RedisURL
	if redisURL == "" {
		redisURL = defaultRedisURL
	}
	redisOptions, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	history, err := openHistory(config.HistoryPath)
	if err != nil {
		return nil, err
	}

	return &Service{
		telegram:      telegram,
		redis:         redis.NewClient(redisOptions),
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		allowedUserID: config.AllowedUserID,
		logger:        logger,
		healthAddr:    config.HealthAddr,
		checkInterval: config.CheckInterval,
		history:       history,
	}, nil
}

func (s *Service) Close() error {
	historyErr := s.history.Close()
	redisErr := s.redis.Close()
	if historyErr != nil {
		return historyErr
	}
	return redisErr
}

func (s *Service) Run(ctx context.Context) error {
	healthAddr := s.healthAddr
	if healthAddr == "" {
		healthAddr = ":8080"
	}
	listener, err := net.Listen("tcp", healthAddr)
	if err != nil {
		return fmt.Errorf("start health server: %w", err)
	}
	healthServer := &http.Server{Handler: healthHandler(ctx), ReadHeaderTimeout: 5 * time.Second}
	healthErrors := make(chan error, 1)
	go func() { healthErrors <- healthServer.Serve(listener) }()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := healthServer.Shutdown(shutdownCtx); err != nil {
			s.logger.Error("failed to stop health server", "error", err)
		}
	}()
	if _, err := s.telegram.Request(tgbotapi.NewSetMyCommands(
		tgbotapi.BotCommand{Command: "start", Description: "Conhecer o bot"},
		tgbotapi.BotCommand{Command: "concursos", Description: "Buscar vagas atualizadas"},
		tgbotapi.BotCommand{Command: "acompanhamentos", Description: "Gerenciar alertas"},
		tgbotapi.BotCommand{Command: "favoritos", Description: "Ver concursos salvos"},
		tgbotapi.BotCommand{Command: "ajuda", Description: "Ver comandos disponíveis"},
	)); err != nil {
		return fmt.Errorf("register Telegram commands: %w", err)
	}

	pollCtx, stopPolling := context.WithCancel(ctx)
	updates := make(chan tgbotapi.Update)
	pollDone := make(chan struct{})
	go func() { defer close(pollDone); s.pollUpdates(pollCtx, updates) }()
	defer func() { stopPolling(); <-pollDone }()
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	monitorDone := make(chan struct{})
	go func() { defer close(monitorDone); s.runMonitor(monitorCtx) }()
	defer func() { stopMonitor(); <-monitorDone }()
	s.logger.Info("Telegram commands registered; waiting for updates")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-healthErrors:
			if err != nil && err != http.ErrServerClosed {
				return fmt.Errorf("health server stopped: %w", err)
			}
			return fmt.Errorf("health server stopped")
		case update, ok := <-updates:
			if !ok {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return fmt.Errorf("Telegram update channel closed")
			}
			s.handleUpdate(ctx, update)
		}
	}
}

func healthHandler(ctx context.Context) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if ctx.Err() != nil {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

func (s *Service) pollUpdates(ctx context.Context, output chan<- tgbotapi.Update) {
	defer close(output)
	config := tgbotapi.UpdateConfig{Timeout: 10}
	for ctx.Err() == nil {
		updates, err := s.telegram.GetUpdates(config)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.logger.Warn("failed to poll Telegram", "error", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		for _, update := range updates {
			if update.UpdateID < config.Offset {
				continue
			}
			config.Offset = update.UpdateID + 1
			select {
			case <-ctx.Done():
				return
			case output <- update:
			}
		}
	}
}

func (s *Service) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	if update.CallbackQuery != nil {
		s.handleCallback(ctx, update.CallbackQuery)
		return
	}
	if update.Message != nil {
		s.handleMessage(ctx, update.Message)
	}
}

func (s *Service) handleMessage(ctx context.Context, message *tgbotapi.Message) {
	if !s.isAuthorizedUser(message.From) {
		s.logger.Warn("unauthorized message ignored")
		return
	}

	command := commandName(message.Text)
	s.logger.Info("received Telegram command", "command", command)
	switch command {
	case "/start", "/ajuda":
		if err := s.sendHTML(message.Chat.ID, message.MessageID, helpMessage()); err != nil {
			s.logger.Error("failed to send help message", "error", err)
		}
	case "/concursos":
		if err := s.sendCargoSelector(message.Chat.ID, message.MessageID); err != nil {
			s.logger.Error("failed to send cargo selector", "error", err)
		}
	case "/acompanhamentos":
		if err := s.sendWatchList(ctx, message.Chat.ID, message.MessageID); err != nil {
			s.logger.Error("failed to list watches", "error", err)
		}
	case "/favoritos":
		if err := s.sendFavoriteList(ctx, message.Chat.ID, message.MessageID); err != nil {
			s.logger.Error("failed to list favorites", "error", err)
		}
	default:
		if err := s.sendHTML(message.Chat.ID, message.MessageID, helpMessage()); err != nil {
			s.logger.Error("failed to send help message", "error", err)
		}
	}
}

func (s *Service) handleCallback(ctx context.Context, callback *tgbotapi.CallbackQuery) {
	if !s.isAuthorizedUser(callback.From) {
		s.logger.Warn("unauthorized callback ignored")
		return
	}
	if callback.Message == nil {
		s.logger.Warn("callback without message ignored")
		return
	}
	if _, err := s.telegram.Request(tgbotapi.NewCallback(callback.ID, "")); err != nil {
		s.logger.Warn("failed to acknowledge callback", "error", err)
	}

	parts := strings.Split(callback.Data, ":")
	if len(parts) > 0 {
		switch parts[0] {
		case "sfav":
			s.handleStateFavorite(ctx, callback.Message, parts)
			return
		case "fdetail", "unfav", "flist":
			s.handleFavoriteCallback(ctx, callback.Message, parts)
			return
		}
	}
	if len(parts) < 2 {
		s.logger.Warn("invalid callback data", "data", callback.Data)
		return
	}
	option, ok := optionByID(parts[1])
	if !ok {
		s.logger.Warn("unknown cargo option", "id", parts[1])
		return
	}

	switch parts[0] {
	case "cargo", "states":
		s.handleStateSelection(ctx, callback.Message, option, parts)
	case "vac":
		s.handleVacancyPage(ctx, callback.Message, option, parts)
	case "watch", "watchadd", "watchremove", "unwatch":
		s.handleWatchCallback(ctx, callback.Message, option, parts)
	case "detail", "fav":
		s.handleDetailCallback(ctx, callback.Message, option, parts)
	default:
		s.logger.Warn("unknown callback action", "action", parts[0])
	}
}

func (s *Service) handleStateSelection(ctx context.Context, message *tgbotapi.Message, option CargoOption, parts []string) {
	page := 0
	if parts[0] == "states" {
		if len(parts) != 3 {
			return
		}
		parsed, err := strconv.Atoi(parts[2])
		if err != nil || parsed < 0 {
			return
		}
		page = parsed
	}

	estados, err := s.fetchEstados(ctx)
	if err != nil {
		s.logger.Error("failed to fetch states", "error", err)
		if sendErr := s.sendHTML(message.Chat.ID, message.MessageID, "<b>Não foi possível carregar os estados agora.</b>\nTente novamente em alguns instantes."); sendErr != nil {
			s.logger.Error("failed to send state error", "error", sendErr)
		}
		return
	}
	if page*statesPageSize >= len(estados) {
		return
	}
	favorites := s.favoriteStates(ctx)
	sort.SliceStable(estados, func(i, j int) bool {
		if favorites[estados[i].Sigla] != favorites[estados[j].Sigla] {
			return favorites[estados[i].Sigla]
		}
		return estados[i].Nome < estados[j].Nome
	})
	if err := s.editHTML(message.Chat.ID, message.MessageID, "<b>Filtre por estado</b>\n"+html.EscapeString(option.Nome)+"\nToque ☆ para priorizar uma UF.", stateKeyboard(option, estados, page, favorites)); err != nil {
		s.logger.Error("failed to show state selector", "error", err)
	}
}

func (s *Service) handleVacancyPage(ctx context.Context, message *tgbotapi.Message, option CargoOption, parts []string) {
	if len(parts) != 4 {
		return
	}
	page, err := strconv.Atoi(parts[3])
	if err != nil || page < 0 {
		return
	}
	uf := parts[2]
	if uf == "all" {
		uf = ""
	} else if !validUF(uf) {
		return
	}

	concursos, err := s.fetchConcursos(ctx, option.URL)
	if err != nil {
		s.logger.Error("failed to fetch vacancies", "cargo", option.ID, "state", uf, "error", err)
		if sendErr := s.sendHTML(message.Chat.ID, message.MessageID, "<b>Não foi possível buscar os concursos agora.</b>\nTente novamente em alguns instantes."); sendErr != nil {
			s.logger.Error("failed to send vacancy error", "error", sendErr)
		}
		return
	}

	filtered := filterByEstado(concursos, uf)
	if page*pageSize >= len(filtered) && len(filtered) > 0 {
		return
	}
	s.logger.Info("vacancies loaded", "cargo", option.ID, "state", uf, "page", page+1, "total", len(filtered))
	stateID := uf
	if stateID == "" {
		stateID = "all"
	}
	watching := s.isWatching(ctx, option.ID, stateID)
	if err := s.editHTML(message.Chat.ID, message.MessageID, concursosMessage(option, uf, filtered, page), resultKeyboard(option, uf, page, filtered, watching)); err != nil {
		s.logger.Error("failed to update vacancy list", "error", err)
	}
}

func (s *Service) fetchConcursos(ctx context.Context, url string) ([]Concurso, error) {
	key := "fetch-concursos:concursos:v1:" + concursoID(url, "")
	cached, err := s.redis.Get(ctx, key).Bytes()
	if err == nil {
		var concursos []Concurso
		if json.Unmarshal(cached, &concursos) == nil {
			return concursos, nil
		}
	}
	return s.fetchConcursosFresh(ctx, url)
}

func (s *Service) fetchConcursosFresh(ctx context.Context, url string) ([]Concurso, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "fetch-concursos/1.0 (+github.com/hsm-gustavo/fetch-concursos)")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	concursos, err := parseConcurso(resp.Body)
	if err != nil {
		return nil, err
	}
	s.enrichConcursos(ctx, url, concursos)
	key := "fetch-concursos:concursos:v1:" + concursoID(url, "")
	if data, err := json.Marshal(concursos); err == nil {
		if err := s.redis.Set(ctx, key, data, concursosCacheTTL).Err(); err != nil {
			s.logger.Warn("failed to cache concursos", "error", err)
		}
	}
	return concursos, nil
}

func (s *Service) fetchEstados(ctx context.Context) ([]Estado, error) {
	cached, err := s.redis.Get(ctx, statesCacheKey).Bytes()
	if err == nil {
		var estados []Estado
		if err := json.Unmarshal(cached, &estados); err == nil {
			s.logger.Debug("states cache hit", "count", len(estados))
			return estados, nil
		}
		s.logger.Warn("invalid states cache; refreshing", "error", err)
	} else if err != redis.Nil {
		s.logger.Warn("Redis unavailable; querying IBGE", "error", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, statesAPIURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var estados []Estado
	if err := json.NewDecoder(resp.Body).Decode(&estados); err != nil {
		return nil, err
	}
	sort.Slice(estados, func(i, j int) bool { return estados[i].Nome < estados[j].Nome })

	encoded, err := json.Marshal(estados)
	if err != nil {
		return nil, err
	}
	if err := s.redis.Set(ctx, statesCacheKey, encoded, statesCacheTTL).Err(); err != nil {
		s.logger.Warn("failed to cache states", "error", err)
	} else {
		s.logger.Info("states cache refreshed", "count", len(estados))
	}
	return estados, nil
}

func parseConcurso(r io.Reader) ([]Concurso, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}
	pagina := findByID(doc, "pagina")
	if pagina == nil {
		return nil, fmt.Errorf("#pagina not found")
	}

	var concursos []Concurso
	var pending *Concurso
	seen := make(map[string]bool)
	for n := pagina.FirstChild; n != nil; n = n.NextSibling {
		if n.Type != html.ElementNode || n.Data != "ul" {
			continue
		}
		switch getAttr(n, "class") {
		case "link-d":
			link := findFirstLink(n)
			if link == nil {
				continue
			}
			orgao, uf := normalizeOrgao(textContent(link))
			noticiaURL := canonicalPCIURL(getAttr(link, "href"), "/noticias/")
			if orgao == "" || noticiaURL == "" {
				pending = nil
				continue
			}
			pending = &Concurso{Orgao: orgao, UF: uf, NoticiaURL: noticiaURL}
		case "link-i":
			link := findFirstLink(n)
			if link == nil || pending == nil {
				continue
			}
			cargo := normalizeSpace(textContent(link))
			cargoURL := canonicalPCIURL(getAttr(link, "href"), "/vagas/")
			if cargo == "" || cargoURL == "" {
				continue
			}
			item := *pending
			item.Cargo = cargo
			item.CargoURL = cargoURL
			item.ID = concursoID(item.NoticiaURL, item.Cargo)
			if !seen[item.ID] {
				concursos = append(concursos, item)
				seen[item.ID] = true
			}
		}
	}
	return concursos, nil
}

var ufSuffix = regexp.MustCompile(`(?i)^(.*?)\s+-\s+([A-Z]{2})$`)

func normalizeSpace(value string) string { return strings.Join(strings.Fields(value), " ") }

func normalizeOrgao(value string) (string, string) {
	value = normalizeSpace(value)
	match := ufSuffix.FindStringSubmatch(value)
	if match == nil || !validUF(strings.ToUpper(match[2])) {
		return value, ""
	}
	return normalizeSpace(match[1]), strings.ToUpper(match[2])
}

func canonicalPCIURL(value, pathPrefix string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Hostname() != "www.pciconcursos.com.br" || parsed.Scheme != "https" || !strings.Contains(parsed.Path, pathPrefix) {
		return ""
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

func concursoID(noticiaURL, cargo string) string {
	sum := sha256.Sum256([]byte(noticiaURL + "\n" + strings.ToLower(normalizeSpace(cargo))))
	return fmt.Sprintf("%x", sum[:12])
}

func findByID(n *html.Node, id string) *html.Node {
	if n.Type == html.ElementNode && getAttr(n, "id") == id {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if found := findByID(child, id); found != nil {
			return found
		}
	}
	return nil
}

func getAttr(n *html.Node, key string) string {
	for _, attr := range n.Attr {
		if attr.Key == key {
			return attr.Val
		}
	}
	return ""
}

func findFirstLink(n *html.Node) *html.Node {
	if n.Type == html.ElementNode && n.Data == "a" {
		return n
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		if link := findFirstLink(child); link != nil {
			return link
		}
	}
	return nil
}

func textContent(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			b.WriteString(node.Data)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func optionByID(id string) (CargoOption, bool) {
	for _, option := range cargoOptions {
		if option.ID == id {
			return option, true
		}
	}
	return CargoOption{}, false
}

func filterByEstado(concursos []Concurso, uf string) []Concurso {
	if uf == "" {
		return concursos
	}
	var filtered []Concurso
	for _, concurso := range concursos {
		if concurso.UF == strings.ToUpper(uf) {
			filtered = append(filtered, concurso)
		}
	}
	return filtered
}

func concursosMessage(option CargoOption, estado string, concursos []Concurso, page int) string {
	start := page * pageSize
	if start >= len(concursos) {
		start = 0
	}
	stateLabel := "Todos os estados"
	if estado != "" {
		stateLabel = estado
	}
	if len(concursos) == 0 {
		return fmt.Sprintf("<b>%s</b>\n%s\n\nNenhuma vaga encontrada no momento.", html.EscapeString(option.Nome), html.EscapeString(stateLabel))
	}

	end := min(start+pageSize, len(concursos))
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>\n%s · %d oportunidades encontradas\nToque no número abaixo para ver detalhes e favoritar.\n\n", html.EscapeString(option.Nome), html.EscapeString(stateLabel), len(concursos))
	for i, concurso := range concursos[start:end] {
		var summary []string
		for _, field := range []string{concurso.Vagas, concurso.Remuneracao, concurso.Escolaridade, concurso.Prazo} {
			if field != "" {
				summary = append(summary, html.EscapeString(field))
			}
		}
		item := fmt.Sprintf("%d. <b>%s</b>\n%s\n", start+i+1, html.EscapeString(concurso.Orgao), html.EscapeString(concurso.Cargo))
		if len(summary) > 0 {
			item += "Publicação: " + strings.Join(summary, " · ") + "\n"
		}
		item += fmt.Sprintf("<a href=\"%s\">Notícia</a> · <a href=\"%s\">Vagas do cargo</a>\n\n", html.EscapeString(concurso.NoticiaURL), html.EscapeString(concurso.CargoURL))
		if b.Len()+len(item) > maxMessageLen {
			fmt.Fprint(&b, "<i>A lista foi encurtada para caber em uma única mensagem.</i>")
			break
		}
		b.WriteString(item)
	}
	return strings.TrimSpace(b.String())
}

func helpMessage() string {
	return "<b>Fetch Concursos</b>\n\nEscolha um cargo para acompanhar as vagas mais recentes.\n\n<b>Comandos disponíveis</b>\n/concursos — escolher um cargo e buscar vagas\n/acompanhamentos — gerenciar alertas\n/favoritos — ver concursos salvos\n/ajuda — mostrar esta mensagem"
}

func (s *Service) sendHTML(chatID int64, replyTo int, text string) error {
	message := tgbotapi.NewMessage(chatID, text)
	message.ParseMode = parseModeHTML
	message.DisableWebPagePreview = true
	message.ReplyToMessageID = replyTo
	_, err := s.telegram.Send(message)
	return err
}

func (s *Service) sendCargoSelector(chatID int64, replyTo int) error {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(cargoOptions[0].Nome, "cargo:"+cargoOptions[0].ID)),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(cargoOptions[1].Nome, "cargo:"+cargoOptions[1].ID)),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(cargoOptions[2].Nome, "cargo:"+cargoOptions[2].ID)),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(cargoOptions[3].Nome, "cargo:"+cargoOptions[3].ID)),
	)
	message := tgbotapi.NewMessage(chatID, "<b>Qual cargo você quer acompanhar?</b>\nEscolha uma opção abaixo.")
	message.ParseMode = parseModeHTML
	message.ReplyToMessageID = replyTo
	message.ReplyMarkup = keyboard
	_, err := s.telegram.Send(message)
	return err
}

func stateKeyboard(option CargoOption, estados []Estado, page int, favorites map[string]bool) tgbotapi.InlineKeyboardMarkup {
	start := page * statesPageSize
	end := min(start+statesPageSize, len(estados))
	rows := [][]tgbotapi.InlineKeyboardButton{tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Todos os estados", "vac:"+option.ID+":all:0"))}
	for _, estado := range estados[start:end] {
		star := "☆"
		if favorites[estado.Sigla] {
			star = "★"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(estado.Nome+" ("+estado.Sigla+")", "vac:"+option.ID+":"+estado.Sigla+":0"),
			tgbotapi.NewInlineKeyboardButtonData(star, "sfav:"+estado.Sigla+":"+option.ID+":"+strconv.Itoa(page)),
		))
	}
	var navigation []tgbotapi.InlineKeyboardButton
	if page > 0 {
		navigation = append(navigation, tgbotapi.NewInlineKeyboardButtonData("‹ Anterior", "states:"+option.ID+":"+strconv.Itoa(page-1)))
	}
	if end < len(estados) {
		navigation = append(navigation, tgbotapi.NewInlineKeyboardButtonData("Próximos ›", "states:"+option.ID+":"+strconv.Itoa(page+1)))
	}
	if len(navigation) > 0 {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(navigation...))
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func emptyInlineKeyboard() tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.InlineKeyboardMarkup{InlineKeyboard: make([][]tgbotapi.InlineKeyboardButton, 0)}
}

func inlineKeyboard(rows [][]tgbotapi.InlineKeyboardButton) tgbotapi.InlineKeyboardMarkup {
	if len(rows) == 0 {
		return emptyInlineKeyboard()
	}
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func resultKeyboard(option CargoOption, uf string, page int, concursos []Concurso, watching bool) tgbotapi.InlineKeyboardMarkup {
	stateID := uf
	if stateID == "" {
		stateID = "all"
	}
	var navigation []tgbotapi.InlineKeyboardButton
	if page > 0 {
		navigation = append(navigation, tgbotapi.NewInlineKeyboardButtonData("‹ Anterior", "vac:"+option.ID+":"+stateID+":"+strconv.Itoa(page-1)))
	}
	if (page+1)*pageSize < len(concursos) {
		navigation = append(navigation, tgbotapi.NewInlineKeyboardButtonData("Próximas ›", "vac:"+option.ID+":"+stateID+":"+strconv.Itoa(page+1)))
	}
	rows := [][]tgbotapi.InlineKeyboardButton{}
	if len(navigation) > 0 {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(navigation...))
	}
	start := page * pageSize
	end := min(start+pageSize, len(concursos))
	if start < end {
		var detailRow []tgbotapi.InlineKeyboardButton
		for index, item := range concursos[start:end] {
			detailRow = append(detailRow, tgbotapi.NewInlineKeyboardButtonData(strconv.Itoa(start+index+1), "detail:"+option.ID+":"+stateID+":"+strconv.Itoa(page)+":"+item.ID))
			if len(detailRow) == 5 {
				rows = append(rows, tgbotapi.NewInlineKeyboardRow(detailRow...))
				detailRow = nil
			}
		}
		if len(detailRow) > 0 {
			rows = append(rows, tgbotapi.NewInlineKeyboardRow(detailRow...))
		}
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Filtrar por estado", "cargo:"+option.ID)))
	watchLabel := "Acompanhar cargo e estado"
	watchAction := "watchadd"
	if watching {
		watchLabel = "Parar acompanhamento"
		watchAction = "watchremove"
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(watchLabel, watchAction+":"+option.ID+":"+stateID)))
	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func (s *Service) editHTML(chatID int64, messageID int, text string, keyboard tgbotapi.InlineKeyboardMarkup) error {
	message := tgbotapi.NewEditMessageText(chatID, messageID, text)
	message.ParseMode = parseModeHTML
	message.DisableWebPagePreview = true
	message.ReplyMarkup = &keyboard
	_, err := s.telegram.Request(message)
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) && apiErr.Code == http.StatusBadRequest && strings.Contains(apiErr.Message, "message is not modified") {
		return nil
	}
	return err
}

func commandName(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}
	return strings.Split(fields[0], "@")[0]
}

func (s *Service) isAuthorizedUser(user *tgbotapi.User) bool {
	return user != nil && user.ID == s.allowedUserID
}
