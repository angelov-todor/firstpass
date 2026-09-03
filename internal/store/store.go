// Package store persists what firstpass has already decided, so a restart or a
// crash never causes a second review of the same pull request.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Outcome is the recorded result for one pull request.
type Outcome string

const (
	OutcomeReviewed       Outcome = "reviewed"
	OutcomeSkippedAuthor  Outcome = "skipped_author"
	OutcomeSkippedState   Outcome = "skipped_state"
	OutcomeSkippedOwner   Outcome = "skipped_owner"
	OutcomeSkippedRepo    Outcome = "skipped_repo"
	OutcomeNeedsAttention Outcome = "needs_attention"
	OutcomeExpired        Outcome = "expired"
	OutcomeInFlight       Outcome = "in_flight"
)

// Terminal reports whether the outcome closes the book on a pull request.
// in_flight is the only non-terminal outcome: finding one on a later sweep is
// how a review that was killed part-way through is detected.
func (o Outcome) Terminal() bool { return o != OutcomeInFlight }

// Review is the record for a pull request firstpass has acted on.
type Review struct {
	Key            string    `json:"key"`
	Outcome        Outcome   `json:"outcome"`
	HeadSHA        string    `json:"head_sha,omitempty"`
	TriggerMessage string    `json:"trigger_message,omitempty"`
	StartedAt      time.Time `json:"started_at,omitempty"`
	DecidedAt      time.Time `json:"decided_at,omitempty"`
	DurationMS     int64     `json:"duration_ms,omitempty"`
	ExitCode       int       `json:"exit_code,omitempty"`
	ReportPath     string    `json:"report_path,omitempty"`
	Detail         string    `json:"detail,omitempty"`
}

// Pending is a pull request deferred to a later sweep.
type Pending struct {
	Key         string    `json:"key"`
	FirstSeen   time.Time `json:"first_seen"`
	Attempts    int       `json:"attempts"`
	LastAttempt time.Time `json:"last_attempt"`
	LastReason  string    `json:"last_reason"`
}

// Watermark is the newest chat message already processed. The message name is
// stable and unique; CreateTime has ties and clock skew, so it is only a coarse
// bound.
type Watermark struct {
	MessageName string    `json:"message_name"`
	CreateTime  time.Time `json:"create_time"`
}

var (
	bucketMeta    = []byte("meta")
	bucketReviews = []byte("reviews")
	bucketPending = []byte("pending")
	keyWatermark  = []byte("watermark")
)

// Store is a bbolt-backed record of past decisions.
type Store struct{ db *bolt.DB }

// Open creates the database and its buckets if they do not exist.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range [][]byte{bucketMeta, bucketReviews, bucketPending} {
			if _, err := tx.CreateBucketIfNotExists(b); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Watermark() (Watermark, bool, error) {
	var w Watermark
	ok, err := s.get(bucketMeta, keyWatermark, &w)
	return w, ok, err
}

func (s *Store) SetWatermark(w Watermark) error { return s.put(bucketMeta, keyWatermark, w) }

func (s *Store) Review(key string) (Review, bool, error) {
	var r Review
	ok, err := s.get(bucketReviews, []byte(key), &r)
	return r, ok, err
}

func (s *Store) PutReview(r Review) error { return s.put(bucketReviews, []byte(r.Key), r) }

// DeleteReview removes a record entirely, forgetting that firstpass ever
// decided anything about the pull request.
//
// Nothing in the pipeline calls this. `firstpass replay` used to, and the
// deletion is exactly why a replay that then failed left the PR with no record
// at all, so the next sweep reviewed it again; replay now ignores the record
// in place instead. Anything reaching for this should be sure it wants the
// dedupe evidence gone rather than overwritten.
func (s *Store) DeleteReview(key string) error { return s.del(bucketReviews, key) }

func (s *Store) Reviews() ([]Review, error) {
	var out []Review
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketReviews).ForEach(func(_, v []byte) error {
			var r Review
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	})
	return out, err
}

func (s *Store) Pending(key string) (Pending, bool, error) {
	var p Pending
	ok, err := s.get(bucketPending, []byte(key), &p)
	return p, ok, err
}

func (s *Store) PutPending(p Pending) error { return s.put(bucketPending, []byte(p.Key), p) }

func (s *Store) DeletePending(key string) error { return s.del(bucketPending, key) }

func (s *Store) AllPending() ([]Pending, error) {
	var out []Pending
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketPending).ForEach(func(_, v []byte) error {
			var p Pending
			if err := json.Unmarshal(v, &p); err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
	})
	return out, err
}

func (s *Store) put(bucket, key []byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Put(key, b)
	})
}

func (s *Store) del(bucket []byte, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucket).Delete([]byte(key))
	})
}

func (s *Store) get(bucket, key []byte, dst any) (bool, error) {
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucket).Get(key)
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, dst)
	})
	if err != nil {
		return false, fmt.Errorf("read %s/%s: %w", bucket, key, err)
	}
	return found, nil
}
