package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"go.klarlabs.de/nomi/internal/configio"
	"go.klarlabs.de/nomi/internal/secrets"
	"go.klarlabs.de/nomi/internal/storage/db"
)

// ConfigServer wires GET /config/export + POST /config/import. Both
// gated by the standard bearer-token middleware (no special auth) —
// the snapshot omits secret plaintext, so the trust boundary matches
// every other authenticated read/write endpoint.
type ConfigServer struct {
	deps configio.Deps
}

func NewConfigServer(database *db.DB, secretStore secrets.Store) *ConfigServer {
	return &ConfigServer{deps: configio.Deps{
		DB:           database,
		Providers:    db.NewProviderProfileRepository(database),
		Assistants:   db.NewAssistantRepository(database),
		Settings:     db.NewAppSettingsRepository(database),
		Globals:      db.NewGlobalSettingsRepository(database),
		Memory:       db.NewMemoryRepository(database),
		PluginStates: db.NewPluginStateRepository(database),
		Secrets:      secretStore,
	}}
}

// Export renders the current daemon state as YAML (Content-Type
// application/x-yaml) so a `curl -O` produces a file ready for `nomi
// import`. Clients that want JSON can parse the YAML themselves;
// keeping one wire format on the server avoids drift between the two.
func (s *ConfigServer) Export(c *gin.Context) {
	snap, err := configio.Export(s.deps)
	if err != nil {
		respondInternal(c, "failed to export config snapshot", err)
		return
	}
	out, err := configio.Marshal(snap)
	if err != nil {
		respondInternal(c, "failed to encode config snapshot", err)
		return
	}
	c.Header("Content-Disposition", `attachment; filename="nomi-config.yaml"`)
	c.Data(http.StatusOK, "application/x-yaml", out)
}

// Import accepts a YAML body (Content-Type application/x-yaml or
// text/yaml; we don't enforce — Marshal/Unmarshal handle anything the
// snapshot's tags accept) and applies it idempotently. Returns the
// per-section counts so the caller can show the user what changed.
func (s *ConfigServer) Import(c *gin.Context) {
	body, err := c.GetRawData()
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	var snap configio.Snapshot
	if err := configio.Unmarshal(body, &snap); err != nil {
		respondValidationError(c, "parse snapshot: "+err.Error())
		return
	}
	res, err := configio.Import(&snap, s.deps)
	if err != nil {
		respondValidationError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"result": res})
}
