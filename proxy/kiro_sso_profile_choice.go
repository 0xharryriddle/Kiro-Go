package proxy

import (
	"fmt"
	"kiro-go/auth"
	"strings"
	"sync"
	"time"
)

const kiroSsoProfileChoiceTTL = 5 * time.Minute

type pendingKiroSsoProfileChoice struct {
	sessionID      string
	credential     auth.KiroSsoResult
	tokenExpiresAt int64
	expiresAt      time.Time
	profiles       map[string]KiroProfile
	warnings       []KiroProfileRegionWarning
	timer          *time.Timer
}

type kiroSsoProfileChoiceStore struct {
	mu      sync.Mutex
	entries map[string]*pendingKiroSsoProfileChoice
	ttl     time.Duration
}

func newKiroSsoProfileChoiceStore(ttl time.Duration) *kiroSsoProfileChoiceStore {
	if ttl <= 0 {
		ttl = kiroSsoProfileChoiceTTL
	}
	return &kiroSsoProfileChoiceStore{
		entries: make(map[string]*pendingKiroSsoProfileChoice),
		ttl:     ttl,
	}
}

func (h *Handler) getKiroSsoProfileChoiceStore() *kiroSsoProfileChoiceStore {
	h.kiroSsoProfileChoicesMu.Lock()
	defer h.kiroSsoProfileChoicesMu.Unlock()
	if h.kiroSsoProfileChoices == nil {
		h.kiroSsoProfileChoices = newKiroSsoProfileChoiceStore(kiroSsoProfileChoiceTTL)
	}
	return h.kiroSsoProfileChoices
}

func (s *kiroSsoProfileChoiceStore) put(sessionID string, credential auth.KiroSsoResult, tokenExpiresAt int64, profiles []KiroProfile, warnings []KiroProfileRegionWarning) (time.Time, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return time.Time{}, fmt.Errorf("session ID is required")
	}
	selectable := make(map[string]KiroProfile)
	for _, profile := range profiles {
		profile.Arn = strings.TrimSpace(profile.Arn)
		profile.Region = strings.ToLower(strings.TrimSpace(profile.Region))
		if profile.Arn == "" || profile.Region == "" || !profile.Usable {
			continue
		}
		selectable[profile.Arn] = profile
	}
	if len(selectable) < 2 {
		return time.Time{}, fmt.Errorf("at least two usable profiles are required")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if previous, ok := s.entries[sessionID]; ok {
		if previous.timer != nil {
			previous.timer.Stop()
		}
		previous.credential = auth.KiroSsoResult{}
		delete(s.entries, sessionID)
	}
	entry := &pendingKiroSsoProfileChoice{
		sessionID:      sessionID,
		credential:     credential,
		tokenExpiresAt: tokenExpiresAt,
		expiresAt:      time.Now().Add(s.ttl),
		profiles:       selectable,
		warnings:       append([]KiroProfileRegionWarning(nil), warnings...),
	}
	s.entries[sessionID] = entry
	entry.timer = time.AfterFunc(s.ttl, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := s.entries[sessionID]; ok && current == entry {
			entry.credential = auth.KiroSsoResult{}
			delete(s.entries, sessionID)
		}
	})
	return entry.expiresAt, nil
}

func (s *kiroSsoProfileChoiceStore) get(sessionID string) ([]KiroProfile, []KiroProfileRegionWarning, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[strings.TrimSpace(sessionID)]
	if !ok {
		return nil, nil, time.Time{}, fmt.Errorf("profile_choice_not_found")
	}
	if !time.Now().Before(entry.expiresAt) {
		s.expireLocked(entry)
		return nil, nil, time.Time{}, fmt.Errorf("profile_choice_expired")
	}
	profiles := make([]KiroProfile, 0, len(entry.profiles))
	for _, profile := range entry.profiles {
		profiles = append(profiles, profile)
	}
	sortKiroProfiles(profiles)
	return profiles, append([]KiroProfileRegionWarning(nil), entry.warnings...), entry.expiresAt, nil
}

func (s *kiroSsoProfileChoiceStore) consume(sessionID, profileArn string) (auth.KiroSsoResult, KiroProfile, int64, error) {
	var credential auth.KiroSsoResult
	var profile KiroProfile
	var tokenExpiresAt int64
	err := s.commit(sessionID, profileArn, func(c auth.KiroSsoResult, p KiroProfile, expiry int64) error {
		credential, profile, tokenExpiresAt = c, p, expiry
		return nil
	})
	return credential, profile, tokenExpiresAt, err
}

// commit serializes valid finalization attempts. The entry is consumed only after
// persist succeeds; a failed persist leaves the credential and original deadline
// intact so the operator may retry without repeating the hosted sign-in.
func (s *kiroSsoProfileChoiceStore) commit(sessionID, profileArn string, persist func(auth.KiroSsoResult, KiroProfile, int64) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[strings.TrimSpace(sessionID)]
	if !ok {
		return fmt.Errorf("profile_choice_not_found")
	}
	if !time.Now().Before(entry.expiresAt) {
		s.expireLocked(entry)
		return fmt.Errorf("profile_choice_expired")
	}
	profile, ok := entry.profiles[strings.TrimSpace(profileArn)]
	if !ok {
		return fmt.Errorf("profile_not_offered")
	}
	if persist == nil {
		return fmt.Errorf("profile_choice_persist_missing")
	}
	if err := persist(entry.credential, profile, entry.tokenExpiresAt); err != nil {
		return err
	}
	entry.credential = auth.KiroSsoResult{}
	delete(s.entries, entry.sessionID)
	if entry.timer != nil {
		entry.timer.Stop()
	}
	return nil
}

func (s *kiroSsoProfileChoiceStore) cancel(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[strings.TrimSpace(sessionID)]
	if !ok {
		return false
	}
	s.expireLocked(entry)
	return true
}

func (s *kiroSsoProfileChoiceStore) expireLocked(entry *pendingKiroSsoProfileChoice) {
	entry.credential = auth.KiroSsoResult{}
	delete(s.entries, entry.sessionID)
	if entry.timer != nil {
		entry.timer.Stop()
	}
}

func sortKiroProfiles(profiles []KiroProfile) {
	for i := 1; i < len(profiles); i++ {
		for j := i; j > 0; j-- {
			left := profiles[j-1].Region + "\x00" + profiles[j-1].Arn
			right := profiles[j].Region + "\x00" + profiles[j].Arn
			if left <= right {
				break
			}
			profiles[j-1], profiles[j] = profiles[j], profiles[j-1]
		}
	}
}
