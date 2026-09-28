package bot

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestIsAuthorizedUser(t *testing.T) {
	service := &Service{allowedUserID: 6244996803}

	for _, tt := range []struct {
		name string
		user *tgbotapi.User
		want bool
	}{
		{name: "authorized user", user: &tgbotapi.User{ID: 6244996803}, want: true},
		{name: "another user", user: &tgbotapi.User{ID: 1}, want: false},
		{name: "missing user", user: nil, want: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := service.isAuthorizedUser(tt.user); got != tt.want {
				t.Fatalf("isAuthorizedUser() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseConcursoFixture(t *testing.T) {
	file, err := os.Open("testdata/cargos.html")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	items, err := parseConcurso(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4", len(items))
	}
	if items[0].Orgao != "Câmara de Assis" || items[0].Cargo != "ANALISTA DE SISTEMAS" || items[0].UF != "SP" {
		t.Fatalf("unexpected first item: %+v", items[0])
	}
	if items[0].NoticiaURL != "https://www.pciconcursos.com.br/noticias/camara-de-assis-sp-abre-concurso-publico-com-salarios-de-ate-8510" || items[0].CargoURL != "https://www.pciconcursos.com.br/concursos/vagas/analista-de-sistemas" {
		t.Fatalf("unexpected URLs: %+v", items[0])
	}
	if items[1].Orgao != items[2].Orgao || items[1].ID == items[2].ID {
		t.Fatalf("two cargos from one publication should have distinct IDs: %+v %+v", items[1], items[2])
	}
	if items[3].UF != "" {
		t.Fatalf("expected unknown UF, got %q", items[3].UF)
	}
	if items[0].ID != concursoID(items[0].NoticiaURL, " analista  de sistemas ") {
		t.Fatal("ID is not normalized")
	}
}

func TestParseConcursoInvalidEntries(t *testing.T) {
	file, err := os.Open("testdata/cargos_invalidos.html")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	items, err := parseConcurso(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].UF != "SE" {
		t.Fatalf("unexpected items: %+v", items)
	}
	if _, err := parseConcurso(strings.NewReader("<html><body></body></html>")); err == nil {
		t.Fatal("expected missing #pagina error")
	}
}

func TestParseVagasFixture(t *testing.T) {
	file, err := os.Open("testdata/vagas.html")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	items, err := parseVagas(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d summaries", len(items))
	}
	summary := items["https://www.pciconcursos.com.br/noticias/camara-de-assis-sp-abre-concurso-publico-com-salarios-de-ate-8510"]
	if summary.UF != "SP" || summary.Vagas != "8 vagas" || summary.Remuneracao != "até R$ 8.510,00" || summary.Escolaridade != "Fundamental / Médio / Técnico / Superior" || summary.Prazo != "08/10/2026" {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestParseNoticiaFixture(t *testing.T) {
	file, err := os.Open("testdata/noticia.html")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	detail, err := parseNoticia(file)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Jornada == "" || detail.Inscricoes == "" || detail.Taxa == "" || detail.Provas == "" || detail.Remuneracao == "" {
		t.Fatalf("missing fields: %+v", detail)
	}
	if detail.LinkInscricao != "https://www.funcamp.unicamp.br/" || detail.LinkEdital != "https://www.funcamp.unicamp.br/edital.pdf" {
		t.Fatalf("unexpected links: %+v", detail)
	}
}

func TestHistoryPersistsEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.sqlite")
	db, err := openHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{history: db, logger: slog.Default()}
	service.recordEvent(context.Background(), "item-1", "favorited", map[string]string{"cargo": "analista-sistemas"})
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var eventType, metadata string
	if err := db.QueryRow("SELECT event_type, metadata_json FROM events WHERE item_id = ?", "item-1").Scan(&eventType, &metadata); err != nil {
		t.Fatal(err)
	}
	if eventType != "favorited" || !strings.Contains(metadata, "analista-sistemas") {
		t.Fatalf("unexpected persisted event: %s %s", eventType, metadata)
	}
}
