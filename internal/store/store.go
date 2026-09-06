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

// Verdict is the review verdict firstpass submitted on a pull request after a
// successful review. It records what was submitted, not merely what the
// reviewer decided: a verdict firstpass declined or failed to submit is not
// one of the two positive values.
type Verdict string

const (
	// VerdictNone is the zero value: there was a verdict and firstpass did
	// not submit it. It covers a dry run, a submission that failed, and every
	// row written before verdicts existed. Deliberately empty, so an old row
	// on disk and a row with nothing submitted are the same state rather than
	// two. It is distinct from VerdictUnknown, which says there was no
	// verdict to submit in the first place.
	VerdictNone Verdict = ""
	// VerdictApproved means firstpass submitted an approving review.
	VerdictApproved Verdict = "approved"
	// VerdictFindings means firstpass submitted a COMMENT review because
	// something Critical or Important was raised.
	VerdictFindings Verdict = "findings"
	// VerdictUnknown means the review ran and its comments are posted, but
	// the reviewer printed no verdict line firstpass recognised, so nothing
	// was submitted and nothing was guessed.
	VerdictUnknown Verdict = "unknown"
	// VerdictWithheld means the reviewer decided approve and firstpass
	// declined to submit it: a human has an outstanding request for changes,
	// or firstpass could not enumerate the feedback already on the pull
	// request and so cannot support "everything raised has been addressed".
	//
	// A distinct value rather than reusing findings or none. Recording it as
	// findings would claim the reviewer raised something it did not; recording
	// it as none would lose the reason entirely, which is the one thing an
	// operator needs when asking why a clean pull request was not approved.
	VerdictWithheld Verdict = "withheld"
)

// Review is the record for a pull request firstpass has acted on.
type Review struct {
	Key            string  `json:"key"`
	Outcome        Outcome `json:"outcome"`
	HeadSHA        string  `json:"head_sha,omitempty"`
	TriggerMessage string  `json:"trigger_message,omitempty"`
	// TriggerTime is when that chat message was posted, as the Chat API
	// reported it. It exists so "was this post made after the one we last
	// reviewed for" can be asked of two values from the *same* clock.
	//
	// Comparing a post time against DecidedAt crosses clock domains: Google
	// timestamps the message, the local machine timestamps the decision. A
	// laptop clock an hour behind is enough to make a post that genuinely
	// predates a review compare as later than it, which turns an ordinary push
	// into an unrequested second pass -- and a watermark gap holds the
	// watermark, so the whole window is re-offered every sweep until the gap
	// is cleared. Post against post, the skew cancels.
	//
	// A row written before this field decodes as zero, which is the signal to
	// fall back to the DecidedAt comparison: an existing row then behaves
	// exactly as it does today, which is the conservative direction.
	//
	// No omitempty: it is inert on a struct type, so the encoder emits the
	// zero time either way. See LastPausedAt on Pending.
	TriggerTime time.Time `json:"trigger_time"`
	// No omitempty on these two: it is inert on a struct type, so the encoder
	// emits the zero time either way. See LastPausedAt below.
	StartedAt  time.Time `json:"started_at"`
	DecidedAt  time.Time `json:"decided_at"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	ExitCode   int       `json:"exit_code,omitempty"`
	ReportPath string    `json:"report_path,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	// Verdict is what firstpass submitted on the pull request. omitempty is
	// load-bearing here, unlike on the time fields above: a row with no
	// verdict must look exactly like the reviewed rows already in the
	// database from before verdicts existed.
	Verdict Verdict `json:"verdict,omitempty"`

	// Pass counts the reviews firstpass has run on this pull request: 1 for a
	// first pass, 2 for the second pass a re-post with new commits triggers,
	// and so on. Read it through PassNumber, never directly: the production
	// database is full of reviewed rows written before this field existed,
	// and every one of them was a first pass.
	//
	// omitempty for the same reason as Verdict: a pass of 0 is a state no
	// code ever means, so keeping it off disk makes a pre-existing row and a
	// freshly written first pass the same shape to read.
	Pass int `json:"pass,omitempty"`
	// PreviousHeadSHA is the commit the pass before this one reviewed, so a
	// second pass does not lose the commit whose comments are already on the
	// pull request. Empty on a first pass.
	//
	// It is redundant with the last-but-one entry of ReviewedSHAs and is kept
	// anyway: it is what makes a row written before ReviewedSHAs existed
	// decode into a usable set (see ReviewedCommits), and the reviewer is told
	// about this one commit specifically rather than about the whole set.
	PreviousHeadSHA string `json:"previous_head_sha,omitempty"`
	// DryRun says this pass ran with dry_run set, and therefore posted
	// nothing: /code-review was invoked without --comment, so its findings
	// went to a report on disk and never reached the pull request.
	//
	// A later pass needs it, because the reviewer is told that the pass before
	// it "posted its findings as inline comments" and asked not to restate
	// them. After a dry run both halves are false, and the second half would
	// suppress every finding for no reason -- on the very report that is the
	// documented gate before going live.
	//
	// Absent on every row written before this field existed, which reads as
	// live. That is the safe direction: assuming a pass posted nothing when it
	// did is what puts a second copy of every comment on a colleague's pull
	// request, and the reverse merely asks for a full review.
	DryRun bool `json:"dry_run,omitempty"`
	// ReviewedSHAs is every commit a pass of this review has reviewed, oldest
	// first, with the current HeadSHA last. Read it through ReviewedCommits.
	//
	// It exists because HeadSHA alone is the *last* pass's commit, and "no
	// pull request is reviewed twice for the same commit" is a statement about
	// all of them. A head force-pushed back to any earlier reviewed commit
	// compares unequal to HeadSHA, so a single-scalar check would review it
	// again and post a second copy of that pass's comments onto the very lines
	// that already carry them -- the headline invariant failing in exactly the
	// way it exists to prevent.
	//
	// It grows by one 40-character string per pass, and a pass needs a re-post
	// with new commits, so the growth is bounded by how often a team re-posts
	// one pull request. omitempty for the same reason as Pass: a row with
	// nothing to say must look like the rows already on disk.
	ReviewedSHAs []string `json:"reviewed_shas,omitempty"`
}

// ReviewedCommits is every commit a pass of this review has reviewed, oldest
// first.
//
// A row written before ReviewedSHAs existed has the set derived from the two
// SHAs it does carry, so an existing record behaves correctly rather than
// letting its first pass's commit back through. That derivation is why
// PreviousHeadSHA is kept despite being redundant on new rows.
func (r Review) ReviewedCommits() []string {
	out := make([]string, 0, len(r.ReviewedSHAs)+2)
	add := func(sha string) {
		if sha == "" {
			return
		}
		for _, s := range out {
			if s == sha {
				return
			}
		}
		out = append(out, sha)
	}
	for _, sha := range r.ReviewedSHAs {
		add(sha)
	}
	// Unioned in rather than trusted to be in the slice already. On a
	// well-formed row they are its last two entries and this changes nothing;
	// on a row where they disagree with it -- a hand-edited database, or a
	// future writer that sets one and forgets the other -- the answer is still
	// every commit the row names, so the function is total instead of relying
	// on an invariant held somewhere else. That is worth a line: the one thing
	// it is asked is whether a commit already carries this tool's comments.
	add(r.PreviousHeadSHA)
	add(r.HeadSHA)
	return out
}

// HasReviewedCommit reports whether a pass of this review has already reviewed
// the given commit -- and therefore whether its comments may already be on
// those exact lines.
//
// An empty sha is never reviewed. Not knowing what the head is cannot be
// allowed to read as "we have seen it", and it cannot read as "we have not"
// either: the caller has to establish the head before asking.
func (r Review) HasReviewedCommit(sha string) bool {
	if sha == "" {
		return false
	}
	for _, s := range r.ReviewedCommits() {
		if s == sha {
			return true
		}
	}
	return false
}

// PassNumber is how many reviews firstpass has run on this pull request,
// counting this one. It exists so an absent Pass -- every reviewed row written
// before the field did -- reads as the first pass it was, rather than as a
// pass 0 that has never existed.
func (r Review) PassNumber() int {
	if r.Pass < 1 {
		return 1
	}
	return r.Pass
}

// Pending is a pull request deferred to a later sweep.
type Pending struct {
	Key         string    `json:"key"`
	FirstSeen   time.Time `json:"first_seen"`
	Attempts    int       `json:"attempts"`
	LastAttempt time.Time `json:"last_attempt"`
	LastReason  string    `json:"last_reason"`

	// TriggerMessage is the chat message that offered this pull request, and
	// TriggerTime is when it was posted. They are the provenance of the park,
	// restored onto the candidate when the ref is re-offered from this bucket.
	//
	// Without them a deferred ref came back anonymous. That was survivable
	// while a review was a once-per-pull-request decision, but a second pass
	// asks "did a post ask for this, and did that post come after the last
	// review" -- so an anonymous ref could never be one. A re-post deferred by
	// a transient gh failure, a draft, a pause or simply the per-sweep cap was
	// therefore lost for good, and its pending row could never be retired
	// either, because the record gate's skip returns above expirePending: it
	// sat in `firstpass status` for ever.
	//
	// A row written before these fields existed decodes with neither, which
	// reads as "no post is known to have asked for this" -- the same as the
	// paths that genuinely have no message behind them, and the safe answer
	// for both.
	//
	// No omitempty on TriggerTime: it is inert on a struct type, so the
	// encoder emits the zero time either way. See LastPausedAt below.
	TriggerMessage string    `json:"trigger_message,omitempty"`
	TriggerTime    time.Time `json:"trigger_time"`

	// LastPausedAt is when this entry was last observed during a paused sweep,
	// zero when the entry is not currently parked by a pause.
	//
	// It is how the pipeline stops the expiry clock during a pause without
	// discarding the age accrued before it: each paused sweep shifts FirstSeen
	// forward by the interval since the previous paused sighting, so only the
	// paused time is excluded. A row written before this field existed decodes
	// as zero, which is exactly right -- it was not parked by a pause.
	//
	// No omitempty: it is inert on a struct type, so the encoder would emit
	// the zero time regardless. Claiming otherwise invites the wrong
	// inference about what an unpaused row looks like on disk.
	LastPausedAt time.Time `json:"last_paused_at"`
}

// Watermark is the newest chat message already processed. The message name is
// stable and unique; CreateTime has ties and clock skew, so it is only a coarse
// bound.
type Watermark struct {
	MessageName string    `json:"message_name"`
	CreateTime  time.Time `json:"create_time"`
}

// MessageRecord is what one chat message carried, kept so the chat reaction
// firstpass puts on that message can be completed by a later sweep -- or a
// later process.
//
// It exists because the reaction is per message, not per pull request: a
// single message routinely carries several PR links, reviews run strictly one
// at a time, and the result reaction is only right once every one of them has
// reached a terminal outcome. That can be hours later, by which time the
// message has scrolled out of the fetch window and the daemon may have been
// restarted, so neither the ref list nor the reaction name can live in memory.
//
// Every field here is additional state. Nothing in this record is ever read to
// decide whether a pull request is reviewed: the reviews bucket alone decides
// that, and a message record that is missing, stale or corrupt can cost a
// reaction and nothing else.
//
// There is deliberately no delete. These records are never pruned, so the
// bucket grows by one small row per chat message that carried a pull request
// link -- the same unbounded growth the reviews bucket already has, one row
// per pull request, and negligible beside it. Pruning settled records would
// discard the very state that stops a message being reacted to twice, and
// while the reviews bucket's own dedupe would probably catch that on its own,
// "probably, via a second mechanism" is not the footing to put an invariant
// on. Not deleting is the cheaper mistake.
type MessageRecord struct {
	Name    string   `json:"name"`
	RefKeys []string `json:"ref_keys"`
	// No omitempty: it is inert on a struct type, so the encoder emits the
	// zero time either way.
	FirstSeen time.Time `json:"first_seen"`

	// WatchApplied records that the 👀 add was attempted, and is the signal
	// that at least one of this message's pull requests actually started being
	// reviewed. It is set before the API call, not after: an outward act that
	// might be repeated is worse than one that is occasionally missed, which
	// is the same discipline as writing a Review as in_flight before claude
	// starts.
	//
	// WatchReaction is the name the API returned, and the only handle for
	// removing the reaction again. It stays empty when the add failed, which
	// is why "has a review started" asks WatchApplied and "is there a 👀 to
	// remove" asks WatchReaction.
	WatchApplied  bool   `json:"watch_applied"`
	WatchReaction string `json:"watch_reaction,omitempty"`

	// ResultApplied records that the ✅ / 💬 add was attempted, and is what
	// stops a second result reaction on the same message. Set before the call
	// for the same reason as WatchApplied.
	ResultApplied  bool   `json:"result_applied"`
	ResultReaction string `json:"result_reaction,omitempty"`
}

var (
	bucketMeta     = []byte("meta")
	bucketReviews  = []byte("reviews")
	bucketPending  = []byte("pending")
	bucketMessages = []byte("messages")
	keyWatermark   = []byte("watermark")
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
		// CreateBucketIfNotExists, not CreateBucket: firstpass was already
		// running in production when the messages bucket was added, so the
		// database on disk has the other three and not this one. Store.get
		// dereferences tx.Bucket() without a nil check, so an absent bucket
		// would panic rather than read as empty.
		for _, b := range [][]byte{bucketMeta, bucketReviews, bucketPending, bucketMessages} {
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

func (s *Store) Message(name string) (MessageRecord, bool, error) {
	var m MessageRecord
	ok, err := s.get(bucketMessages, []byte(name), &m)
	return m, ok, err
}

func (s *Store) PutMessage(m MessageRecord) error { return s.put(bucketMessages, []byte(m.Name), m) }

func (s *Store) AllMessages() ([]MessageRecord, error) {
	var out []MessageRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketMessages).ForEach(func(_, v []byte) error {
			var m MessageRecord
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			out = append(out, m)
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
