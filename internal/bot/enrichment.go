package bot

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

type VagaResumo struct {
	UF           string
	Vagas        string
	Remuneracao  string
	Escolaridade string
	Prazo        string
}

var moneyPattern = regexp.MustCompile(`R\$\s*[0-9][0-9.,]*`)

func findByClass(node *html.Node, class string) *html.Node {
	if node.Type == html.ElementNode {
		for _, name := range strings.Fields(getAttr(node, "class")) {
			if name == class {
				return node
			}
		}
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if found := findByClass(child, class); found != nil {
			return found
		}
	}
	return nil
}

func firstText(node *html.Node) string {
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		if child.Type == html.TextNode {
			return normalizeSpace(child.Data)
		}
	}
	return ""
}

func parseVagas(reader io.Reader) (map[string]VagaResumo, error) {
	doc, err := html.Parse(reader)
	if err != nil {
		return nil, err
	}
	root := findByID(doc, "concursos")
	if root == nil {
		return nil, fmt.Errorf("#concursos not found")
	}
	result := make(map[string]VagaResumo)
	for card := root.FirstChild; card != nil; card = card.NextSibling {
		if card.Type != html.ElementNode || !hasClass(card, "na") {
			continue
		}
		url := canonicalPCIURL(getAttr(card, "data-url"), "/noticias/")
		if url == "" {
			continue
		}
		var summary VagaResumo
		if cc := findByClass(card, "cc"); cc != nil {
			uf := strings.ToUpper(normalizeSpace(textContent(cc)))
			if validUF(uf) {
				summary.UF = uf
			}
		}
		if cd := findByClass(card, "cd"); cd != nil {
			opening := firstText(cd)
			if money := moneyPattern.FindString(opening); money != "" {
				if before, after, ok := strings.Cut(opening, "até"); ok {
					summary.Remuneracao = "até " + strings.TrimSpace(after)
					summary.Vagas = normalizeSpace(before)
				} else {
					summary.Remuneracao = money
					summary.Vagas = normalizeSpace(strings.TrimSpace(strings.Replace(opening, money, "", 1)))
				}
			} else {
				summary.Vagas = opening
			}
			for child := cd.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.ElementNode && child.Data == "span" {
					for nested := child.FirstChild; nested != nil; nested = nested.NextSibling {
						if nested.Type == html.ElementNode && nested.Data == "span" {
							summary.Escolaridade = normalizeSpace(textContent(nested))
							break
						}
					}
				}
			}
		}
		if ce := findByClass(card, "ce"); ce != nil {
			summary.Prazo = normalizeSpace(textContent(ce))
		}
		result[url] = summary
	}
	return result, nil
}

func hasClass(node *html.Node, class string) bool {
	for _, name := range strings.Fields(getAttr(node, "class")) {
		if name == class {
			return true
		}
	}
	return false
}

func (s *Service) enrichConcursos(ctx context.Context, cargoURL string, items []Concurso) {
	vagasURL := strings.Replace(cargoURL, "/cargos/", "/vagas/", 1)
	if vagasURL == cargoURL {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, vagasURL, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "fetch-concursos/1.0 (+github.com/hsm-gustavo/fetch-concursos)")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		s.logger.Warn("failed to fetch vacancy summaries", "error", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.logger.Warn("vacancy summaries unavailable", "status", resp.StatusCode)
		return
	}
	summaries, err := parseVagas(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		s.logger.Warn("failed to parse vacancy summaries", "error", err)
		return
	}
	for i := range items {
		summary, ok := summaries[items[i].NoticiaURL]
		if !ok {
			continue
		}
		if items[i].UF == "" {
			items[i].UF = summary.UF
		}
		items[i].Vagas = summary.Vagas
		items[i].Remuneracao = summary.Remuneracao
		items[i].Escolaridade = summary.Escolaridade
		items[i].Prazo = summary.Prazo
	}
}
