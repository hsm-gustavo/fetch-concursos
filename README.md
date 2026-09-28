# Fetch Concursos

Bot privado do Telegram para consultar concursos de informática publicados no PCI Concursos. É possível filtrar por UF, acompanhar combinações de cargo e estado, receber alertas de novas oportunidades e salvar favoritos.

## Executar

1. Crie um bot no Telegram e copie `.env.example` para `.env`.
2. Preencha `TELEGRAM_APITOKEN` e `TELEGRAM_ALLOWED_USER_ID` com o token e o ID da conta autorizada.
3. Execute `docker compose -f prod.compose.yaml up --build -d`.

O endpoint `http://127.0.0.1:8080/healthz` responde `200` enquanto o processo está ativo. Ajuste `HEALTH_PORT` se a porta local estiver ocupada. `CHECK_INTERVAL` usa durações de Go, como `30m` ou `1h`, com mínimo de um minuto. O histórico SQLite fica no volume `history-data`; Redis armazena os dados operacionais no volume `redis-data`.

## Comandos

- `/concursos`: escolher cargo e UF, ver resultados e abrir detalhes.
- `/acompanhamentos`: listar e remover alertas. Na lista de concursos, use **Acompanhar cargo e estado** para ativar um alerta. As publicações já presentes são marcadas como conhecidas antes de ativá-lo.
- `/favoritos`: listar concursos salvos. Abra uma vaga pelo botão numerado para favoritar ou desfavoritar.
- `/ajuda`: mostrar os comandos.

No seletor de estados, o botão ☆ marca uma UF favorita e a move para o início da lista. Esse favorito não ativa alertas e não salva concursos.

Os resultados de `/cargos/...` são cacheados por cinco minutos. O monitor ignora esse cache, busca o PCI diretamente e o renova. Os resumos de `/vagas/...` são mesclados apenas quando a URL da notícia coincide; vagas, remuneração e escolaridade podem se referir à publicação inteira. A página `/noticias/...` é consultada apenas ao abrir um detalhe.

## Desenvolvimento

Use `go test ./...` para rodar os testes locais. As amostras HTML do PCI estão em `internal/bot/testdata`. O código requer Go 1.26.8.
