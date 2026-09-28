package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const favoriteStatesKey = "fetch-concursos:favorite-states:v1"
const favoriteConcursosKey = "fetch-concursos:favorite-concursos:v1"

func (s *Service) favoriteStates(ctx context.Context) map[string]bool {
	result := make(map[string]bool)
	states, err := s.redis.SMembers(ctx, favoriteStatesKey).Result()
	if err != nil {
		s.logger.Warn("failed to load favorite states", "error", err)
		return result
	}
	for _, uf := range states {
		if validUF(uf) {
			result[uf] = true
		}
	}
	return result
}

func (s *Service) handleStateFavorite(ctx context.Context, message *tgbotapi.Message, parts []string) {
	if len(parts) != 4 || !validUF(parts[1]) {
		return
	}
	option, ok := optionByID(parts[2])
	if !ok {
		return
	}
	page, err := strconv.Atoi(parts[3])
	if err != nil || page < 0 {
		return
	}
	if s.favoriteStates(ctx)[parts[1]] {
		err = s.redis.SRem(ctx, favoriteStatesKey, parts[1]).Err()
	} else {
		err = s.redis.SAdd(ctx, favoriteStatesKey, parts[1]).Err()
	}
	if err != nil {
		s.logger.Error("failed to update favorite state", "error", err)
		return
	}
	s.handleStateSelection(ctx, message, option, []string{"states", option.ID, strconv.Itoa(page)})
}

func (s *Service) favoriteConcursos(ctx context.Context) ([]Concurso, error) {
	values, err := s.redis.HGetAll(ctx, favoriteConcursosKey).Result()
	if err != nil {
		return nil, err
	}
	items := make([]Concurso, 0, len(values))
	for _, value := range values {
		var item Concurso
		if json.Unmarshal([]byte(value), &item) == nil && item.ID != "" {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Orgao < items[j].Orgao })
	return items, nil
}

func (s *Service) favoriteList(ctx context.Context) (string, tgbotapi.InlineKeyboardMarkup, error) {
	items, err := s.favoriteConcursos(ctx)
	if err != nil {
		return "", tgbotapi.InlineKeyboardMarkup{}, err
	}
	if len(items) == 0 {
		return "<b>Favoritos</b>\n\nNenhum concurso salvo. Abra uma vaga em /concursos para favoritar.", emptyInlineKeyboard(), nil
	}
	var b strings.Builder
	b.WriteString("<b>Favoritos</b>\n\n")
	var rows [][]tgbotapi.InlineKeyboardButton
	for i, item := range items {
		line := fmt.Sprintf("%d. %s · %s (%s)\n", i+1, htmlEscape(item.Orgao), htmlEscape(item.Cargo), htmlEscape(displayUF(item.UF)))
		if b.Len()+len(line) > maxMessageLen {
			break
		}
		b.WriteString(line)
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData(strconv.Itoa(i+1)+" · Ver", "fdetail:"+item.ID),
			tgbotapi.NewInlineKeyboardButtonData("Remover", "unfav:"+item.ID),
		))
	}
	return strings.TrimSpace(b.String()), inlineKeyboard(rows), nil
}

func (s *Service) sendFavoriteList(ctx context.Context, chatID int64, replyTo int) error {
	text, keyboard, err := s.favoriteList(ctx)
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

func (s *Service) editFavoriteList(ctx context.Context, chatID int64, messageID int) error {
	text, keyboard, err := s.favoriteList(ctx)
	if err != nil {
		return err
	}
	return s.editHTML(chatID, messageID, text, keyboard)
}

func (s *Service) handleFavoriteCallback(ctx context.Context, message *tgbotapi.Message, parts []string) {
	if len(parts) == 1 && parts[0] == "flist" {
		if err := s.editFavoriteList(ctx, message.Chat.ID, message.MessageID); err != nil {
			s.logger.Error("failed to show favorites", "error", err)
		}
		return
	}
	if len(parts) != 2 {
		return
	}
	if parts[0] == "unfav" {
		if err := s.redis.HDel(ctx, favoriteConcursosKey, parts[1]).Err(); err != nil {
			s.logger.Error("failed to remove favorite", "error", err)
			return
		}
		s.recordEvent(ctx, parts[1], "unfavorited", nil)
		if err := s.editFavoriteList(ctx, message.Chat.ID, message.MessageID); err != nil {
			s.logger.Error("failed to show favorites", "error", err)
		}
		return
	}
	if parts[0] == "fdetail" {
		value, err := s.redis.HGet(ctx, favoriteConcursosKey, parts[1]).Result()
		if err != nil {
			return
		}
		var item Concurso
		if json.Unmarshal([]byte(value), &item) != nil || item.ID != parts[1] {
			return
		}
		s.showDetail(ctx, message, item, "flist")
	}
}

func (s *Service) handleDetailCallback(ctx context.Context, message *tgbotapi.Message, option CargoOption, parts []string) {
	if len(parts) != 5 || (parts[2] != "all" && !validUF(parts[2])) {
		return
	}
	page, err := strconv.Atoi(parts[3])
	if err != nil || page < 0 {
		return
	}
	items, err := s.fetchConcursos(ctx, option.URL)
	if err != nil {
		s.logger.Error("failed to load detail item", "error", err)
		return
	}
	var item *Concurso
	for i := range items {
		if items[i].ID == parts[4] && (parts[2] == "all" || items[i].UF == parts[2]) {
			item = &items[i]
			break
		}
	}
	if item == nil {
		_ = s.sendHTML(message.Chat.ID, message.MessageID, "Esta vaga não está mais na lista atual.")
		return
	}
	if parts[0] == "fav" {
		exists, err := s.redis.HExists(ctx, favoriteConcursosKey, item.ID).Result()
		if err != nil {
			s.logger.Error("failed to check favorite", "error", err)
			return
		}
		if exists {
			err = s.redis.HDel(ctx, favoriteConcursosKey, item.ID).Err()
		} else {
			data, marshalErr := json.Marshal(item)
			if marshalErr != nil {
				return
			}
			err = s.redis.HSet(ctx, favoriteConcursosKey, item.ID, data).Err()
		}
		if err != nil {
			s.logger.Error("failed to update favorite", "error", err)
			return
		}
		if exists {
			s.recordEvent(ctx, item.ID, "unfavorited", nil)
		} else {
			s.recordEvent(ctx, item.ID, "favorited", map[string]string{"cargo": option.ID, "uf": item.UF})
		}
	}
	back := "vac:" + option.ID + ":" + parts[2] + ":" + strconv.Itoa(page)
	s.showDetail(ctx, message, *item, back)
}

func (s *Service) showDetail(ctx context.Context, message *tgbotapi.Message, item Concurso, back string) {
	isFavorite, err := s.redis.HExists(ctx, favoriteConcursosKey, item.ID).Result()
	if err != nil {
		s.logger.Warn("failed to read favorite status", "error", err)
	}
	text := fmt.Sprintf("<b>%s</b>\n%s · %s\n\n", htmlEscape(item.Cargo), htmlEscape(item.Orgao), htmlEscape(displayUF(item.UF)))
	if item.Vagas != "" || item.Remuneracao != "" || item.Escolaridade != "" || item.Prazo != "" {
		text += "<b>Resumo da publicação</b>\n"
	}
	if item.Vagas != "" {
		text += "Vagas: " + htmlEscape(item.Vagas) + "\n"
	}
	if item.Remuneracao != "" {
		text += "Remuneração: " + htmlEscape(item.Remuneracao) + "\n"
	}
	if item.Escolaridade != "" {
		text += "Escolaridade: " + htmlEscape(item.Escolaridade) + "\n"
	}
	if item.Prazo != "" {
		text += "Prazo: " + htmlEscape(item.Prazo) + "\n"
	}
	if detail, err := s.fetchNoticia(ctx, item); err == nil {
		text += "\n<b>Informações da notícia</b>\n"
		for _, field := range []struct{ label, value string }{
			{"Jornada", detail.Jornada}, {"Inscrições", detail.Inscricoes}, {"Taxa", detail.Taxa}, {"Provas", detail.Provas}, {"Remuneração", detail.Remuneracao},
		} {
			if field.value != "" && len(text)+len(field.value) < maxMessageLen-300 {
				text += "<b>" + field.label + ":</b> " + htmlEscape(field.value) + "\n"
			}
		}
		if detail.LinkInscricao != "" {
			text += "<a href=\"" + htmlEscape(detail.LinkInscricao) + "\">Site de inscrição</a>\n"
		}
		if detail.LinkEdital != "" {
			text += "<a href=\"" + htmlEscape(detail.LinkEdital) + "\">Edital</a>\n"
		}
	} else {
		s.logger.Warn("failed to fetch news detail", "id", item.ID, "error", err)
		text += "\nDetalhes adicionais indisponíveis no momento.\n"
	}
	text += "\n<a href=\"" + htmlEscape(item.NoticiaURL) + "\">Abrir notícia</a>"
	rows := [][]tgbotapi.InlineKeyboardButton{}
	if strings.HasPrefix(back, "vac:") {
		parts := strings.Split(back, ":")
		label := "☆ Favoritar"
		if isFavorite {
			label = "★ Desfavoritar"
		}
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData(label, "fav:"+parts[1]+":"+parts[2]+":"+parts[3]+":"+item.ID)))
	}
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonURL("Abrir notícia", item.NoticiaURL)))
	rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("‹ Voltar", back)))
	if err := s.editHTML(message.Chat.ID, message.MessageID, text, tgbotapi.NewInlineKeyboardMarkup(rows...)); err != nil {
		s.logger.Error("failed to show detail", "error", err)
	}
}
