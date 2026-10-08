package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

type pathMapper []projectMapping

func newPathMapper(mappings []projectMapping) pathMapper {
	var mapped pathMapper
	for _, mapping := range mappings {
		if mapping.New != "" {
			mapped = append(mapped, mapping)
		}
	}
	sort.SliceStable(mapped, func(i, j int) bool { return len(mapped[i].Old) > len(mapped[j].Old) })
	return mapped
}

// mapPath maps a path under a mapped project to its new location; paths under no
// mapped project are returned unchanged.
func (m pathMapper) mapPath(path string) (string, bool) {
	for _, mapping := range m {
		if path == mapping.Old {
			return mapping.New, true
		}
		if strings.HasPrefix(path, mapping.Old+"/") {
			return mapping.New + path[len(mapping.Old):], true
		}
	}
	return path, false
}

func (m pathMapper) movesAnything() bool {
	for _, mapping := range m {
		if mapping.Old != mapping.New {
			return true
		}
	}
	return false
}

func encodeJSONString(value string) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return bytes.TrimRight(buffer.Bytes(), "\n")
}

// rewriteTopLevelString replaces the value of one top-level string field of a JSON
// object line and leaves every other byte as it was. Lines that are not JSON
// objects, or have no such field, come back unchanged.
func rewriteTopLevelString(line []byte, key string, mapPath func(string) (string, bool)) []byte {
	body := bytes.TrimRight(line, "\r\n")
	ending := line[len(body):]
	if !json.Valid(body) {
		return line
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return line
	}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return line
		}
		name, ok := token.(string)
		if !ok {
			return line
		}
		if name != key {
			var skipped json.RawMessage
			if err := decoder.Decode(&skipped); err != nil {
				return line
			}
			continue
		}
		before := decoder.InputOffset()
		token, err = decoder.Token()
		if err != nil {
			return line
		}
		value, ok := token.(string)
		if !ok {
			return line
		}
		after := decoder.InputOffset()
		mapped, changed := mapPath(value)
		if !changed || mapped == value {
			return line
		}
		quote := bytes.IndexByte(body[before:after], '"')
		if quote < 0 {
			return line
		}
		var out bytes.Buffer
		out.Write(body[:before+int64(quote)])
		out.Write(encodeJSONString(mapped))
		out.Write(body[after:])
		out.Write(ending)
		return out.Bytes()
	}
	return line
}

// mergeHistory puts the restored prompt-history lines that are not already present
// before the local ones, which are newer.
func mergeHistory(local, restored []byte, mapper pathMapper) ([]byte, int) {
	present := map[string]bool{}
	for _, line := range completeLines(local) {
		present[string(line)] = true
	}
	var merged bytes.Buffer
	added := 0
	for _, line := range completeLines(restored) {
		line = rewriteTopLevelString(line, "project", mapper.mapPath)
		if present[string(line)] {
			continue
		}
		present[string(line)] = true
		merged.Write(line)
		added++
	}
	if len(local) > 0 && !bytes.HasSuffix(local, []byte("\n")) {
		local = append(local, '\n')
	}
	merged.Write(local)
	return merged.Bytes(), added
}

func decodeSettings(data []byte) (map[string]any, error) {
	settings := map[string]any{}
	if len(bytes.TrimSpace(data)) == 0 {
		return settings, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&settings); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("unexpected data after the settings object")
	}
	if settings == nil {
		settings = map[string]any{}
	}
	return settings, nil
}

func remapHome(value any, oldHome, newHome string) any {
	switch v := value.(type) {
	case string:
		if oldHome == "" || oldHome == newHome {
			return v
		}
		if v == oldHome {
			return newHome
		}
		return strings.ReplaceAll(v, oldHome+"/", newHome+"/")
	case []any:
		for i := range v {
			v[i] = remapHome(v[i], oldHome, newHome)
		}
		return v
	case map[string]any:
		for key := range v {
			v[key] = remapHome(v[key], oldHome, newHome)
		}
		return v
	default:
		return v
	}
}

// mergeSettings adds the restored settings the local file does not have; local
// values always win. cleanupDays, when set, raises cleanupPeriodDays to at least
// that many days.
func mergeSettings(local []byte, restored []byte, oldHome, newHome string, cleanupDays int) ([]byte, []string, bool, error) {
	localSettings, err := decodeSettings(local)
	if err != nil {
		return nil, nil, false, fmt.Errorf("your settings file is not valid JSON, so it was left as it is: %w", err)
	}
	var added []string
	if restored != nil {
		restoredSettings, err := decodeSettings(restored)
		if err != nil {
			return nil, nil, false, fmt.Errorf("the backed-up settings are not valid JSON: %w", err)
		}
		for key, value := range restoredSettings {
			if _, present := localSettings[key]; !present {
				localSettings[key] = remapHome(value, oldHome, newHome)
				added = append(added, key)
			}
		}
	}
	changed := len(added) > 0
	if cleanupDays > 0 && settingNumber(localSettings["cleanupPeriodDays"]) < cleanupDays {
		localSettings["cleanupPeriodDays"] = json.Number(strconv.Itoa(cleanupDays))
		changed = true
	}
	sort.Strings(added)
	if !changed {
		return local, nil, false, nil
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(localSettings); err != nil {
		return nil, nil, false, err
	}
	return buffer.Bytes(), added, true, nil
}

func settingNumber(value any) int {
	number, ok := value.(json.Number)
	if !ok {
		return 0
	}
	parsed, err := number.Int64()
	if err != nil {
		return 0
	}
	return int(parsed)
}

// mergeBoot folds a restored claude-afterlife snapshot into a local one with the
// same boot id, keeping whichever copy of each session was seen last.
func mergeBoot(local *Boot, restored Boot, mapper pathMapper) Boot {
	for id, session := range restored.Sessions {
		session.Cwd, _ = mapper.mapPath(session.Cwd)
		restored.Sessions[id] = session
	}
	if local == nil {
		return restored
	}
	merged := *local
	if merged.Sessions == nil {
		merged.Sessions = map[string]Session{}
	}
	for id, session := range restored.Sessions {
		if existing, ok := merged.Sessions[id]; !ok || session.LastSeen.After(existing.LastSeen) {
			merged.Sessions[id] = session
		}
	}
	running := map[string]bool{}
	for _, id := range append(merged.Running, restored.Running...) {
		running[id] = true
	}
	merged.Running = merged.Running[:0]
	for id := range running {
		merged.Running = append(merged.Running, id)
	}
	sort.Strings(merged.Running)
	if restored.UpdatedAt.After(merged.UpdatedAt) {
		merged.UpdatedAt = restored.UpdatedAt
	}
	return merged
}
