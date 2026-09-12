package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"unicode"
	"unicode/utf8"
)

type idMapper struct {
	forward map[string]string
	reverse map[string]string
}

func newIDMapper() *idMapper {
	return &idMapper{forward: make(map[string]string), reverse: make(map[string]string)}
}

// mapID keeps short IDs unchanged. Longer IDs receive a deterministic compact
// representation so a maximum-size sync remains within the socket limit.
// Reverse tracking turns an improbable hash collision into a diagnosable error
// instead of silently merging two runs.
func (m *idMapper) mapID(value string) (string, error) {
	if value == "" || !utf8.ValidString(value) {
		return "", errors.New("identifier is invalid")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return "", errors.New("identifier contains a control character")
		}
	}
	if mapped, exists := m.forward[value]; exists {
		return mapped, nil
	}
	mapped := value
	if len(mapped) > 24 {
		digest := sha256.Sum256([]byte(value))
		mapped = "id-" + hex.EncodeToString(digest[:12])
	}
	if original, exists := m.reverse[mapped]; exists && original != value {
		return "", errors.New("identifier mapping collision")
	}
	m.forward[value] = mapped
	m.reverse[mapped] = value
	return mapped, nil
}

func (m *idMapper) mapRun(run Run) (Run, error) {
	var err error
	if run.AgentID, err = m.mapID(run.AgentID); err != nil {
		return Run{}, err
	}
	if run.RunID, err = m.mapID(run.RunID); err != nil {
		return Run{}, err
	}
	if run.ParentAgentID != "" {
		if run.ParentAgentID, err = m.mapID(run.ParentAgentID); err != nil {
			return Run{}, err
		}
	}
	return run, nil
}
