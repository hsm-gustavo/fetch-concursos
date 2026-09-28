package bot

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"golang.org/x/net/html"
)

const watchesKey = "fetch-concursos:watches:v1"
const notifiedKey = "fetch-concursos:notified:v1"

var ufSet = map[string]bool{"AC": true, "AL": true, "AP": true, "AM": true, "BA": true, "CE": true, "DF": true, "ES": true, "GO": true, "MA": true, "MT": true, "MS": true, "MG": true, "PA": true, "PB": true, "PR": true, "PE": true, "PI": true, "RJ": true, "RN": true, "RS": true, "RO": true, "RR": true, "SC": true, "SP": true, "SE": true, "TO": true}

func validUF(uf string) bool         { return ufSet[uf] }
func htmlEscape(value string) string { return html.EscapeString(value) }

func watchID(cargoID, uf string) string { return cargoID + ":" + uf }
func seenKey(id string) string          { return "fetch-concursos:seen:v1:" + id }

func parseWatchID(id string) (CargoOption, string, bool) {
	parts := strings.Split(id, ":")
	if len(parts) != 2 {
		return CargoOption{}, "", false
	}
	option, ok := optionByID(parts[0])
	if !ok || (parts[1] != "all" && !validUF(parts[1])) {
		return CargoOption{}, "", false
	}
	return option, parts[1], true
}

func (s *Service) isWatching(ctx context.Context, cargoID, uf string) bool {
	watching, err := s.redis.SIsMember(ctx, watchesKey, watchID(cargoID, uf)).Result()
	if err != nil {
		s.logger.Warn("failed to read watches", "error", err)
	}
	return watching
}

func (s *Service) setWatch(ctx context.Context, option CargoOption, uf string, enabled bool) error {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	id := watchID(option.ID, uf)
	if !enabled {
		return s.redis.SRem(ctx, watchesKey, id).Err()
	}
	if s.isWatching(ctx, option.ID, uf) {
		return nil
	}
	items, err := s.fetchConcursosFresh(ctx, option.URL)
	if err != nil {
		return err
	}
	filtered := filterByEstado(items, stateFilter(uf))
	pipe := s.redis.TxPipeline()
	for _, item := range filtered {
		pipe.SAdd(ctx, seenKey(id), item.ID)
	}
	pipe.SAdd(ctx, watchesKey, id)
	_, err = pipe.Exec(ctx)
	return err
}

func (s *Service) handleWatchCallback(ctx context.Context, message *tgbotapi.Message, option CargoOption, parts []string) {
	if len(parts) != 3 || (parts[2] != "all" && !validUF(parts[2])) {
		return
	}
	uf := parts[2]
	if parts[0] != "watch" {
		enabled := parts[0] == "watchadd"
		if err := s.setWatch(ctx, option, uf, enabled); err != nil {
			s.logger.Error("failed to update watch", "error", err)
			_ = s.sendHTML(message.Chat.ID, message.MessageID, "<b>Não foi possível atualizar o acompanhamento.</b> Tente novamente.")
			return
		}
	}
	if parts[0] == "unwatch" {
		if err := s.editWatchList(ctx, message.Chat.ID, message.MessageID); err != nil {
			s.logger.Error("failed to update watches list", "error", err)
		}
		return
	}
	s.handleVacancyPage(ctx, message, option, []string{"vac", option.ID, uf, "0"})
}

func (s *Service) watchList(ctx context.Context) (string, tgbotapi.InlineKeyboardMarkup, error) {
	ids, err := s.redis.SMembers(ctx, watchesKey).Result()
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "<b>Acompanhamentos</b>\n\nNenhum acompanhamento ativo. Use /concursos para começar.", emptyInlineKeyboard(), nil
	}
	var text strings.Builder
	text.WriteString("<b>Acompanhamentos</b>\n\n")
	var rows [][]tgbotapi.InlineKeyboardButton
	for _, id := range ids {
		option, uf, ok := parseWatchID(id)
		if !ok {
			continue
		}
		label := uf
		if uf == "all" {
			label = "Todos os estados"
		}
		fmt.Fprintf(&text, "• %s · %s\n", htmlEscape(option.Nome), htmlEscape(label))
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Remover "+option.Nome+" · "+label, "unwatch:"+id)))
	}
	return strings.TrimSpace(text.String()), inlineKeyboard(rows), nil
}

func (s *Service) sendWatchList(ctx context.Context, chatID int64, replyTo int) error {
	text, keyboard, err := s.watchList(ctx)
	if err != nil {
		return err
	}
	message := tgbotapi.NewMessage(chatID, text)
	message.ParseMode = parseModeHTML
	message.ReplyToMessageID = replyTo
	message.ReplyMarkup = keyboard
	_, err = s.telegram.Send(message)
	return err
}

func (s *Service) editWatchList(ctx context.Context, chatID int64, messageID int) error {
	text, keyboard, err := s.watchList(ctx)
	if err != nil {
		return err
	}
	return s.editHTML(chatID, messageID, text, keyboard)
}

func (s *Service) runMonitor(ctx context.Context) {
	interval := s.checkInterval
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	s.checkWatches(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkWatches(ctx)
		}
	}
}

func (s *Service) checkWatches(ctx context.Context) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	ids, err := s.redis.SMembers(ctx, watchesKey).Result()
	if err != nil {
		s.logger.Error("failed to load watches", "error", err)
		return
	}
	byCargo := make(map[string][]string)
	for _, id := range ids {
		option, _, ok := parseWatchID(id)
		if ok {
			byCargo[option.ID] = append(byCargo[option.ID], id)
		}
	}
	for cargoID, subscriptions := range byCargo {
		if ctx.Err() != nil {
			return
		}
		option, _ := optionByID(cargoID)
		items, err := s.fetchConcursosFresh(ctx, option.URL)
		if err != nil {
			s.logger.Error("monitor fetch failed", "cargo", cargoID, "error", err)
			continue
		}
		for _, id := range subscriptions {
			_, uf, _ := parseWatchID(id)
			for _, item := range filterByEstado(items, stateFilter(uf)) {
				if ctx.Err() != nil {
					return
				}
				added, err := s.redis.SAdd(ctx, seenKey(id), item.ID).Result()
				if err != nil {
					s.logger.Error("failed to mark concurso seen", "error", err)
					break
				}
				if added == 0 {
					continue
				}
				s.recordEvent(ctx, item.ID, "discovered", map[string]string{"watch": id, "cargo": option.ID, "uf": item.UF})
				firstNotification, err := s.redis.SAdd(ctx, notifiedKey, item.ID).Result()
				if err != nil {
					s.logger.Error("failed to deduplicate notification", "error", err)
					continue
				}
				if firstNotification == 0 {
					continue
				}
				message := fmt.Sprintf("<b>Novo concurso: %s</b>\n%s · %s\n<a href=\"%s\">Abrir notícia</a>", htmlEscape(option.Nome), htmlEscape(item.Orgao), htmlEscape(displayUF(item.UF)), htmlEscape(item.NoticiaURL))
				if err := s.sendHTML(s.allowedUserID, 0, message); err != nil {
					s.logger.Error("failed to notify concurso", "id", item.ID, "error", err)
					if removeErr := s.redis.SRem(ctx, notifiedKey, item.ID).Err(); removeErr != nil {
						s.logger.Error("failed to release notification", "id", item.ID, "error", removeErr)
					}
					if removeErr := s.redis.SRem(ctx, seenKey(id), item.ID).Err(); removeErr != nil {
						s.logger.Error("failed to release seen item", "id", item.ID, "error", removeErr)
					}
				} else {
					s.recordEvent(ctx, item.ID, "notified", map[string]string{"watch": id})
				}
			}
		}
	}
}

func stateFilter(uf string) string {
	if uf == "all" {
		return ""
	}
	return uf
}
func displayUF(uf string) string {
	if uf == "" {
		return "UF não informada"
	}
	return uf
}
