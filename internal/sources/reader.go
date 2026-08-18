// Package sources carrega e valida a lista de fontes (imobiliárias) a serem
// raspadas, a partir do arquivo apontado por config.SourcesFile.
//
// O pacote não faz nenhuma requisição de rede: valida apenas o formato das URLs.
// Se um host está no ar ou permite a coleta é responsabilidade dos pacotes
// httpclient e robots.
package sources

import (
	"bufio"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
)

// commentPrefix marca uma linha inteira como comentário. Só vale no início da
// linha (após TrimSpace): "#" também é um caractere válido dentro de uma URL
// (fragmento), então tratá-lo como comentário no meio da linha mutilaria fontes
// legítimas.
const commentPrefix = "#"

// ReadSources lê o arquivo de fontes e devolve as URLs válidas, na ordem em que
// aparecem no arquivo e sem duplicatas.
//
// Linhas em branco e comentários (linhas iniciadas por "#") são ignorados.
// Linhas que não são URLs http/https absolutas são descartadas com um aviso no
// log em vez de abortarem a leitura: uma linha digitada errada não deve impedir
// a coleta de todas as outras fontes. O erro é reservado para falhas do arquivo
// como um todo (inexistente, sem permissão, ilegível).
//
// Um arquivo válido mas sem nenhuma fonte utilizável devolve slice vazio e erro
// nulo — cabe ao chamador decidir se isso é um problema.
func ReadSources(filePath string) ([]string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("sources: could not open sources file %q: %w", filePath, err)
	}
	defer file.Close()

	var (
		urls       []string
		seen       = make(map[string]struct{})
		scanner    = bufio.NewScanner(file)
		lineNum    int
		skipped    int
		duplicates int
	)

	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, commentPrefix) {
			continue
		}

		if err := validate(line); err != nil {
			skipped++
			slog.Warn("sources: skipping invalid source line",
				"file", filePath,
				"line", lineNum,
				"value", line,
				"error", err,
			)
			continue
		}

		if _, exists := seen[line]; exists {
			duplicates++
			slog.Warn("sources: skipping duplicate source line",
				"file", filePath,
				"line", lineNum,
				"value", line,
			)
			continue
		}

		seen[line] = struct{}{}
		urls = append(urls, line)
	}

	// scanner.Err() cobre erros de I/O e também linhas maiores que o buffer
	// padrão (bufio.ErrTooLong). Nos dois casos o arquivo foi lido pela metade,
	// então devolver a lista parcial esconderia fontes perdidas.
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("sources: could not read sources file %q: %w", filePath, err)
	}

	slog.Info("sources loaded",
		"file", filePath,
		"valid", len(urls),
		"invalid", skipped,
		"duplicates", duplicates,
	)

	return urls, nil
}

// validate garante que a linha é uma URL absoluta http/https com host. Sem essa
// checagem, url.Parse aceitaria valores como "imobiliaria.com.br" (que vira um
// path relativo) e o scraper só descobriria o problema na hora da requisição.
func validate(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}

	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("unsupported scheme %q, want http or https", parsed.Scheme)
	}

	if parsed.Host == "" {
		return fmt.Errorf("URL has no host")
	}

	return nil
}
