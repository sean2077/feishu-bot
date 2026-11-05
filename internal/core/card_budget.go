package core

import "encoding/json"

// CardPayloadLimit is deliberately below 30 KiB. The same conservative budget
// applies to inline IM cards and CardKit so a permission fallback is safe.
const CardPayloadLimit = 30000

// CardPayloadBytes measures the serialized card nested in a JSON string, as
// used by both IM content and CardKit data. The reservation covers recipient,
// reply, UUID, sequence and envelope fields; JSON escaping is counted twice.
// It is a wire-size budget, not a count of Unicode characters.
func CardPayloadBytes(card Args) (int, error) {
	raw, err := json.Marshal(card)
	if err != nil {
		return 0, err
	}
	wire, err := json.Marshal(struct {
		Content string `json:"content"`
	}{Content: string(raw)})
	if err != nil {
		return 0, err
	}
	return len(wire) + 1024, nil
}
