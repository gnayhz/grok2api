package inference

import (
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func resourceBenchmarkWeb(cipher security.Cryptor, states repository.ResponseRepository) provider.Adapter {
	return webprovider.NewAdapter(webprovider.Config{}, nil, cipher, historyapp.NewResponseResources(states), nil)
}
