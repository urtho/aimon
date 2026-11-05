package providers

import (
	"context"
	"fmt"

	"github.com/algonode/aimon/internal/config"
	"github.com/algonode/aimon/internal/model"
)

// elevenLabs reports the plan's monthly credits from
// https://api.elevenlabs.io/v1/user/subscription. The API calls them characters;
// the balance is in credits, not dollars. The key needs the user_read permission.
type elevenLabs struct {
	label  string
	apiKey string
	base   string
}

func newElevenLabs(m config.Monitor) *elevenLabs {
	base := m.BaseURL
	if base == "" {
		base = "https://api.elevenlabs.io"
	}
	return &elevenLabs{label: m.Label, apiKey: m.APIKey, base: base}
}

func (e *elevenLabs) ID() string    { return "elevenlabs" }
func (e *elevenLabs) Label() string { return e.label }
func (e *elevenLabs) Kind() string  { return model.KindCredit }

type elSubscription struct {
	Tier           string `json:"tier"`
	CharacterCount int64  `json:"character_count"`
	CharacterLimit int64  `json:"character_limit"`
	NextResetUnix  int64  `json:"next_character_count_reset_unix"`
}

func (e *elevenLabs) Fetch(ctx context.Context) (model.Status, error) {
	if e.apiKey == "" {
		return model.Status{}, fmt.Errorf("missing api_key")
	}
	var body elSubscription
	code, raw, err := getJSON(ctx, e.base+"/v1/user/subscription", map[string]string{"xi-api-key": e.apiKey}, &body)
	if code != 200 {
		// Checked first: an error page need not be JSON, and its status says more.
		return model.Status{}, fmt.Errorf("elevenlabs %d: %s", code, snippet(raw, 160))
	}
	if err != nil {
		return model.Status{}, err
	}
	if body.CharacterLimit <= 0 {
		return model.Status{}, fmt.Errorf("elevenlabs returned no character_limit")
	}
	st := model.Status{
		Provider: e.ID(),
		Label:    e.label,
		Kind:     e.Kind(),
		OK:       true,
		Unit:     "credits",
		Plan:     body.Tier,
		Balance:  fptr(float64(body.CharacterLimit - body.CharacterCount)),
		Total:    fptr(float64(body.CharacterLimit)),
		Used:     fptr(float64(body.CharacterCount)),
	}
	if body.NextResetUnix > 0 {
		st.ResetAt = &body.NextResetUnix
	}
	return st, nil
}
