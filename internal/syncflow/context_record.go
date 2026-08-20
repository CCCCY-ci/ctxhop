package syncflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/CCCCY-ci/agentsync/internal/project"
)

const (
	workspaceContextKind       = "workspace-difference"
	workspaceContextVersion    = 1
	maxWorkspaceContextFiles   = 64
	maxWorkspaceContextPathLen = 256
	maxWorkspaceContextNoteLen = 256
	maxWorkspaceContextBytes   = 32 << 10
)

type workspaceContextRecord struct {
	Type      string                  `json:"type"`
	IsMeta    bool                    `json:"isMeta"`
	Message   workspaceContextMessage `json:"message"`
	AgentSync workspaceContextMarker  `json:"agentsync"`
}

type workspaceContextMessage struct {
	Content string `json:"content"`
}

type workspaceContextMarker struct {
	Kind    string `json:"kind"`
	Version int    `json:"version"`
}

func workspaceContextRecordFor(report project.Report) ([]byte, error) {
	return workspaceContextRecordForAgent("", report)
}

func workspaceContextRecordForAgent(agent string, report project.Report) ([]byte, error) {
	if agent == "codex" {
		return codexWorkspaceContextRecordFor(report)
	}
	return workspaceContextRecordForClaude(report)
}

func workspaceContextRecordForClaude(report project.Report) ([]byte, error) {
	if report.Verdict == project.Consistent {
		return nil, nil
	}

	files := append([]project.FileReport(nil), report.Files...)
	sort.Slice(files, func(i, j int) bool {
		if files[i].Path != files[j].Path {
			return files[i].Path < files[j].Path
		}
		return files[i].Note < files[j].Note
	})

	var content strings.Builder
	switch report.Verdict {
	case project.Explainable:
		content.WriteString("AgentSync restored this session while the target workspace had explainable differences from the recorded state.")
	case project.Divergent:
		content.WriteString("AgentSync restored this session after the target workspace was explicitly accepted as divergent from the recorded state.")
	default:
		content.WriteString("AgentSync restored this session with a non-consistent workspace verdict.")
	}
	content.WriteString(" Re-read the affected files before relying on the previous context.")
	for i, file := range files {
		if i >= maxWorkspaceContextFiles {
			fmt.Fprintf(&content, " Additional differences omitted: %d.", len(files)-i)
			break
		}
		path := contextText(file.Path, maxWorkspaceContextPathLen)
		if path == "" {
			path = "<unknown file>"
		}
		note := contextText(file.Note, maxWorkspaceContextNoteLen)
		if note == "" {
			note = "the file differs from the recorded state"
		}
		content.WriteString("\n- ")
		content.WriteString(path)
		content.WriteString(": ")
		content.WriteString(note)
	}

	record := workspaceContextRecord{
		Type:    "user",
		IsMeta:  true,
		Message: workspaceContextMessage{Content: content.String()},
		AgentSync: workspaceContextMarker{
			Kind:    workspaceContextKind,
			Version: workspaceContextVersion,
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode workspace context: %w", err)
	}
	if len(data) > maxWorkspaceContextBytes {
		return nil, fmt.Errorf("workspace context exceeds %d bytes", maxWorkspaceContextBytes)
	}
	return data, nil
}

func codexWorkspaceContextRecordFor(report project.Report) ([]byte, error) {
	claude, err := workspaceContextRecordForClaude(report)
	if err != nil || claude == nil {
		return claude, err
	}
	var source struct {
		Message workspaceContextMessage `json:"message"`
	}
	if err := json.Unmarshal(claude, &source); err != nil {
		return nil, fmt.Errorf("decode workspace context: %w", err)
	}
	record := codexWorkspaceContextRecord{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "event_msg",
		Payload: codexWorkspaceContextPayload{
			Type:      "user_message",
			Message:   source.Message.Content,
			AgentSync: workspaceContextMarker{Kind: workspaceContextKind, Version: workspaceContextVersion},
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		return nil, fmt.Errorf("encode Codex workspace context: %w", err)
	}
	return data, nil
}

type codexWorkspaceContextRecord struct {
	Timestamp string                       `json:"timestamp"`
	Type      string                       `json:"type"`
	Payload   codexWorkspaceContextPayload `json:"payload"`
}

type codexWorkspaceContextPayload struct {
	Type      string                 `json:"type"`
	Message   string                 `json:"message"`
	AgentSync workspaceContextMarker `json:"agentsync"`
}

func isWorkspaceContextRecord(raw []byte) bool {
	var record struct {
		Type      string                       `json:"type"`
		IsMeta    bool                         `json:"isMeta"`
		AgentSync workspaceContextMarker       `json:"agentsync"`
		Payload   codexWorkspaceContextPayload `json:"payload"`
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return false
	}
	if record.Type == "user" && record.IsMeta && record.AgentSync.Kind == workspaceContextKind && record.AgentSync.Version == workspaceContextVersion {
		return true
	}
	return record.Type == "event_msg" && record.Payload.Type == "user_message" && record.Payload.AgentSync.Kind == workspaceContextKind && record.Payload.AgentSync.Version == workspaceContextVersion
}

func contextText(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-1]) + "..."
}
