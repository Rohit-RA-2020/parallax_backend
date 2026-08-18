package elevenlabs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

type Voice struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Description     string   `json:"description,omitempty"`
	Languages       []string `json:"languages,omitempty"`
	Characteristics []string `json:"characteristics,omitempty"`
}

type VoiceCatalog struct {
	voices []Voice
}

func LoadVoiceCatalog(path string) (*VoiceCatalog, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return &VoiceCatalog{}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &VoiceCatalog{}, nil
		}
		return nil, fmt.Errorf("load ElevenLabs voice catalog: %w", err)
	}
	var voices []Voice
	if err := json.Unmarshal(b, &voices); err != nil {
		return nil, fmt.Errorf("decode ElevenLabs voice catalog: %w", err)
	}
	seen := map[string]bool{}
	clean := make([]Voice, 0, len(voices))
	for i, voice := range voices {
		voice.ID = strings.TrimSpace(voice.ID)
		voice.Name = strings.TrimSpace(voice.Name)
		voice.Description = strings.TrimSpace(voice.Description)
		if voice.ID == "" || voice.Name == "" {
			return nil, fmt.Errorf("voice catalog entry %d requires id and name", i)
		}
		if seen[voice.ID] {
			return nil, fmt.Errorf("voice catalog contains duplicate id %q", voice.ID)
		}
		seen[voice.ID] = true
		voice.Languages = normalizeStrings(voice.Languages)
		voice.Characteristics = normalizeStrings(voice.Characteristics)
		clean = append(clean, voice)
	}
	return &VoiceCatalog{voices: clean}, nil
}

func (c *VoiceCatalog) List(query, language, characteristic string) []Voice {
	if c == nil {
		return nil
	}
	query = strings.ToLower(strings.TrimSpace(query))
	language = strings.ToLower(strings.TrimSpace(language))
	characteristic = strings.ToLower(strings.TrimSpace(characteristic))
	out := make([]Voice, 0, len(c.voices))
	for _, voice := range c.voices {
		if language != "" && !containsFold(voice.Languages, language) {
			continue
		}
		if characteristic != "" && !containsFold(voice.Characteristics, characteristic) {
			continue
		}
		if query != "" {
			haystack := strings.ToLower(strings.Join([]string{voice.ID, voice.Name, voice.Description, strings.Join(voice.Languages, " "), strings.Join(voice.Characteristics, " ")}, " "))
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		out = append(out, voice)
	}
	return out
}

func (c *VoiceCatalog) Get(id string) (Voice, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return Voice{}, errors.New("voice_id is required")
	}
	if c == nil {
		return Voice{}, fmt.Errorf("voice_id %q is not in the configured catalog", id)
	}
	for _, voice := range c.voices {
		if voice.ID == id {
			return voice, nil
		}
	}
	return Voice{}, fmt.Errorf("voice_id %q is not in the configured catalog", id)
}

func normalizeStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[strings.ToLower(value)] {
			continue
		}
		seen[strings.ToLower(value)] = true
		out = append(out, value)
	}
	return out
}

func containsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(value, want) {
			return true
		}
	}
	return false
}
