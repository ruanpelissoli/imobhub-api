// Package cache concentra o acesso ao Redis. Expõe apenas a criação do client
// já validado; as operações de leitura e escrita (set/get, TTL, chaves) vivem
// nos pacotes que consomem o cache.
package cache

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/redis/go-redis/v9"
)

// pingTimeout limita a validação inicial da conexão para que uma URL apontando
// para um host inalcançável falhe rápido, em vez de travar o startup.
const pingTimeout = 5 * time.Second

// New cria o client do Redis a partir da connection string e valida a
// conectividade com um Ping. Em caso de erro o client é fechado antes do
// retorno, então o chamador só precisa chamar Close quando err == nil — mesmo
// contrato de db.Connect.
//
// O ctx recebido governa a validação inicial: cancelá-lo (SIGTERM, por
// exemplo) aborta um Ping pendurado antes do pingTimeout.
func New(ctx context.Context, redisURL string) (*redis.Client, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, parseError(err)
	}

	client := redis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()

	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("cache: could not reach redis (%s): %w", endpoint(opts), err)
	}

	return client, nil
}

// parseError embrulha a falha de redis.ParseURL garantindo que a URL crua nunca
// chegue à mensagem — ela carrega a senha, como a DATABASE_URL.
//
// A redação é necessária porque url.Parse devolve um *url.Error cujo campo URL
// é a string original inteira. Nesse caminho o erro original não é embrulhado
// com %w: preservá-lo manteria a URL acessível por errors.As para qualquer
// handler que a logasse.
func parseError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("cache: invalid REDIS_URL: %s: %v", urlErr.Op, urlErr.Err)
	}
	return fmt.Errorf("cache: invalid REDIS_URL: %w", err)
}

// endpoint descreve o destino da conexão para mensagens de erro usando apenas o
// que é seguro logar. É derivado do *redis.Options parseado, e não da URL, de
// propósito: opts.Password fica de fora por construção.
func endpoint(opts *redis.Options) string {
	return fmt.Sprintf("addr %q, db %d", opts.Addr, opts.DB)
}
