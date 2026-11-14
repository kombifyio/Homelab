package guardpower

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/techstack/internal/privatefs"
)

const (
	journalFile     = "events.jsonl"
	journalMaxBytes = 4 << 20
	journalMaxLines = 512
)

// Event is one durable entry of the power journal. A record is written before
// the effect it names (a running step or a host request) and again with its
// measured outcome, so a crash between the two leaves the intent visible.
type Event struct {
	SchemaVersion  string    `json:"schema_version"`
	Sequence       int64     `json:"sequence"`
	At             time.Time `json:"at"`
	BootID         string    `json:"boot_id,omitempty"`
	Kind           string    `json:"kind"`
	Phase          Phase     `json:"phase"`
	Step           StepName  `json:"step,omitempty"`
	Outcome        Outcome   `json:"outcome,omitempty"`
	Detail         string    `json:"detail,omitempty"`
	OnBattery      *bool     `json:"on_battery,omitempty"`
	ChargePercent  *float64  `json:"charge_percent,omitempty"`
	RuntimeSeconds *int64    `json:"runtime_seconds,omitempty"`
	CancelReason   string    `json:"cancel_reason,omitempty"`
}

// Journal is the node-side, append-only evidence of the power protection. It
// lives in the Guard's admitted custody directory and is fsynced per record.
type Journal struct {
	mu      sync.Mutex
	root    *os.Root
	file    *os.File
	size    int64
	seq     int64
	records []Event
}

// OpenJournalRoot opens or creates the power journal below admitted node
// custody. The caller owns the returned journal and closes it.
func OpenJournalRoot(custody *os.Root, name string) (*Journal, error) {
	if custody == nil {
		return nil, errors.New("guardpower: the journal needs admitted custody")
	}
	root, err := privatefs.OpenChild(custody, name)
	if err != nil {
		return nil, err
	}
	journal := &Journal{root: root}
	if err := journal.load(); err != nil {
		_ = root.Close()
		return nil, err
	}
	return journal, nil
}

func (j *Journal) load() error {
	if raw, err := privatefs.ReadFile(j.root, journalFile); err == nil {
		if len(raw) > 0 && raw[len(raw)-1] != '\n' {
			// A torn tail is not a record. Truncate to the last complete line
			// so the next append cannot be glued onto an unreadable record.
			if index := bytes.LastIndexByte(raw, '\n'); index >= 0 {
				raw = raw[:index+1]
			} else {
				raw = nil
			}
			if err := privatefs.Replace(j.root, journalFile, raw, 0o600); err != nil {
				return err
			}
		}
		j.size = int64(len(raw))
		scanner := bufio.NewScanner(bytes.NewReader(raw))
		scanner.Buffer(make([]byte, 0, 4096), 256<<10)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var event Event
			if json.Unmarshal(line, &event) != nil || event.SchemaVersion != JournalSchema || event.Sequence <= j.seq {
				// A record that cannot be read is not evidence; the journal
				// keeps what it can prove.
				continue
			}
			j.seq = event.Sequence
			j.records = append(j.records, event)
		}
		if len(j.records) > journalMaxLines {
			j.records = j.records[len(j.records)-journalMaxLines:]
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := privatefs.OpenFile(j.root, journalFile, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	j.file = file
	if directory, err := j.root.Open("."); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

// Append writes one event and fsyncs it before returning.
func (j *Journal) Append(event Event) (Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.file == nil {
		return Event{}, errors.New("guardpower: the journal is closed")
	}
	if j.size >= journalMaxBytes {
		if err := j.compactLocked(); err != nil {
			return Event{}, err
		}
	}
	if j.size >= journalMaxBytes {
		return Event{}, errors.New("guardpower: the power journal is full")
	}
	event.SchemaVersion = JournalSchema
	event.Sequence = j.seq + 1
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	event.Detail = boundText(event.Detail, maxDetailBytes)
	event.CancelReason = boundText(event.CancelReason, maxDetailBytes)
	line, err := json.Marshal(event)
	if err != nil {
		return Event{}, err
	}
	line = append(line, '\n')
	written, err := j.file.Write(line)
	if err != nil {
		return Event{}, err
	}
	if err := j.file.Sync(); err != nil {
		return Event{}, err
	}
	j.size += int64(written)
	j.seq = event.Sequence
	j.records = append(j.records, event)
	if len(j.records) > journalMaxLines {
		j.records = j.records[len(j.records)-journalMaxLines:]
	}
	return event, nil
}

// compactLocked keeps the newest half of the bounded in-memory records and
// republishes them atomically. The caller holds the mutex.
func (j *Journal) compactLocked() error {
	keep := j.records
	if len(keep) > journalMaxLines/2 {
		keep = keep[len(keep)-journalMaxLines/2:]
	}
	var buffer bytes.Buffer
	for _, event := range keep {
		line, err := json.Marshal(event)
		if err != nil {
			return err
		}
		buffer.Write(line)
		buffer.WriteByte('\n')
	}
	if j.file != nil {
		if err := j.file.Close(); err != nil {
			return err
		}
		j.file = nil
	}
	if err := privatefs.Replace(j.root, journalFile, buffer.Bytes(), 0o600); err != nil {
		return err
	}
	file, err := privatefs.OpenFile(j.root, journalFile, os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	j.file, j.size, j.records = file, int64(buffer.Len()), append([]Event{}, keep...)
	return nil
}

// Records returns a copy of the bounded in-memory journal.
func (j *Journal) Records() []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Event{}, j.records...)
}

// Close closes the journal file and its custody root.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	var err error
	if j.file != nil {
		err = j.file.Close()
		j.file = nil
	}
	if j.root != nil {
		if closeErr := j.root.Close(); err == nil {
			err = closeErr
		}
		j.root = nil
	}
	return err
}

// boundText trims and bounds a secret-free detail.
func boundText(text string, limit int) string {
	text = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(text, "\x00", ""), "\n", " "))
	if len(text) > limit {
		return text[:limit]
	}
	return text
}
