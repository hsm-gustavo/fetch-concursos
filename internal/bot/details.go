package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type NoticiaDetalhes struct {
	Jornada       string
	Inscricoes    string
	Taxa          string
	Provas        string
	Remuneracao   string
	LinkInscricao string
	LinkEdital    string
}

func parseNoticia(reader io.Reader) (NoticiaDetalhes, error) {
	doc, err := html.Parse(reader)
	if err != nil {
		return NoticiaDetalhes{}, err
	}
	article := findByID(doc, "noticia")
	if article == nil {
		return NoticiaDetalhes{}, fmt.Errorf("article#noticia not found")
	}
	var body *html.Node
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if body != nil {
			return
		}
		if node.Type == html.ElementNode && getAttr(node, "itemprop") == "articleBody" {
			body = node
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(article)
	if body == nil {
		return NoticiaDetalhes{}, fmt.Errorf("article body not found")
	}
	var detail NoticiaDetalhes
	for node := body.FirstChild; node != nil; node = node.NextSibling {
		if node.Type != html.ElementNode || node.Data != "p" {
			continue
		}
		paragraph := normalizeSpace(textContent(node))
		lower := strings.ToLower(paragraph)
		if detail.Jornada == "" && strings.Contains(lower, "jornada") {
			detail.Jornada = paragraph
		}
		if detail.Inscricoes == "" && strings.Contains(lower, "inscriç") && (strings.Contains(lower, "devem") || strings.Contains(lower, "período")) {
			detail.Inscricoes = paragraph
			if link := findFirstExternalLink(node); link != "" {
				detail.LinkInscricao = link
			}
		}
		if detail.Taxa == "" && strings.Contains(lower, "taxa de inscriç") {
			detail.Taxa = paragraph
		}
		if detail.Provas == "" && (strings.Contains(lower, "prova") || strings.Contains(lower, "seleção será")) {
			detail.Provas = paragraph
		}
		if detail.Remuneracao == "" && (strings.Contains(lower, "salário") || strings.Contains(lower, "remuneraç")) {
			detail.Remuneracao = paragraph
		}
		if strings.Contains(lower, "edital") && detail.LinkEdital == "" {
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.ElementNode && child.Data == "a" && strings.Contains(strings.ToLower(textContent(child)), "edital") {
					url := getAttr(child, "href")
					if strings.HasPrefix(url, "https://") {
						detail.LinkEdital = url
					}
				}
			}
		}
	}
	return detail, nil
}

func findFirstExternalLink(node *html.Node) string {
	if node.Type == html.ElementNode && node.Data == "a" {
		url := getAttr(node, "href")
		if strings.HasPrefix(url, "https://") && !strings.Contains(url, "pciconcursos.com.br") {
			return url
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if url := findFirstExternalLink(child); url != "" {
			return url
		}
	}
	return ""
}

func (s *Service) fetchNoticia(ctx context.Context, item Concurso) (NoticiaDetalhes, error) {
	key := "fetch-concursos:noticia:v1:" + item.ID
	if cached, err := s.redis.Get(ctx, key).Bytes(); err == nil {
		var detail NoticiaDetalhes
		if json.Unmarshal(cached, &detail) == nil {
			return detail, nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, item.NoticiaURL, nil)
	if err != nil {
		return NoticiaDetalhes{}, err
	}
	req.Header.Set("User-Agent", "fetch-concursos/1.0 (+github.com/hsm-gustavo/fetch-concursos)")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return NoticiaDetalhes{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return NoticiaDetalhes{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}
	detail, err := parseNoticia(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return NoticiaDetalhes{}, err
	}
	if data, err := json.Marshal(detail); err == nil {
		_ = s.redis.Set(ctx, key, data, time.Hour).Err()
	}
	return detail, nil
}
