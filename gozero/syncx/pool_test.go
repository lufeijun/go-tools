package syncx

import (
	"net/http"
	"testing"
	"time"
)

type HTTPClientPool struct {
	pool *Pool
}

func NewHTTPClientPool(maxClients int) *HTTPClientPool {
	return &HTTPClientPool{
		pool: NewPool(
			maxClients,
			func() any {
				return &http.Client{
					Timeout: 30 * time.Second,
					Transport: &http.Transport{
						MaxIdleConnsPerHost: 100,
					},
				}
			},
			func(x any) {
				if client, ok := x.(*http.Client); ok {
					client.CloseIdleConnections()
				}
			},
			WithMaxAge(10*time.Minute),
		),
	}
}

func (p *HTTPClientPool) Get(url string) (*http.Response, error) {
	client := p.pool.Get().(*http.Client)
	defer p.pool.Put(client)

	return client.Get(url)
}

func TestAdd(t *testing.T) {

}
