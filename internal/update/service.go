package update

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Modes of the update setting.
const (
	Off    = "off"    // never contact GitHub
	Notify = "notify" // check and tell the user
	Auto   = "auto"   // also install when nothing is running
)

// Status is what the interface shows about updates.
type Status struct {
	Current     string     `json:"current"`
	Mode        string     `json:"mode"`
	Checked     *time.Time `json:"checked"`
	Available   bool       `json:"available"`
	Latest      *Release   `json:"latest"`
	Installing  bool       `json:"installing"`
	Error       string     `json:"error"`
	Blocked     string     `json:"blocked"`     // why this build cannot replace itself
	UpdatedFrom string     `json:"updatedFrom"` // the version before the update that started this process
}

// Service checks for releases on a schedule and installs them when asked or,
// in automatic mode, when nothing is running.
type Service struct {
	Updater *Updater
	Mode    func() string // the current setting
	Idle    func() bool   // whether a restart would interrupt nothing
	Changed func(Status)  // called whenever the status changes
	Restart func()        // switch to the freshly installed program

	mu          sync.Mutex
	updatedFrom string // the version this process replaced, if an update started it
	updatedAt   time.Time
	latest      *Release
	checked     *time.Time
	err         string
	installing  bool
	failed      string // a version whose automatic install failed; not retried unasked
	installed   string // a version already put in place, waiting for the restart

	blockedOnce sync.Once
	blocked     string
}

// The service wakes up often but asks GitHub rarely: once a day, or an hour
// after a check that failed.
var (
	firstCheck = time.Minute
	wakeEvery  = 10 * time.Minute
	checkEvery = 24 * time.Hour
	retryCheck = time.Hour
	retryIdle  = 2 * time.Minute
)

// Status reports the current state.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		Current: s.Updater.Current, Mode: s.Mode(), Checked: s.checked, Latest: s.latest,
		Installing: s.installing, Error: s.err,
	}
	if time.Since(s.updatedAt) < 10*time.Minute {
		st.UpdatedFrom = s.updatedFrom
	}
	st.Available = s.latest != nil && s.Updater.Newer(*s.latest)
	s.blockedOnce.Do(func() { s.blocked = s.Updater.Blocked() })
	st.Blocked = s.blocked
	return st
}

// UpdatedFrom records that an update from version from started this process.
// The interface is told for the next few minutes.
func (s *Service) UpdatedFrom(from string) {
	s.mu.Lock()
	s.updatedFrom, s.updatedAt = from, time.Now()
	s.mu.Unlock()
}

func (s *Service) changed() {
	if s.Changed != nil {
		s.Changed(s.Status())
	}
}

// Check looks for a release now.
func (s *Service) Check(ctx context.Context) Status {
	rel, err := s.Updater.Latest(ctx)
	now := time.Now()
	s.mu.Lock()
	s.checked, s.err = &now, ""
	switch {
	case errors.Is(err, ErrNoRelease):
		s.latest = nil
	case err != nil:
		s.err = "Could not check for updates: " + err.Error()
	default:
		s.latest = &rel
	}
	s.mu.Unlock()
	s.changed()
	return s.Status()
}

// Install puts the latest release in place and restarts into it. It returns
// once the download has been verified and swapped in, or with the reason it
// was not.
func (s *Service) Install(ctx context.Context) error {
	s.mu.Lock()
	rel := s.latest
	if s.installing {
		s.mu.Unlock()
		return errors.New("an update is already being installed")
	}
	if rel == nil || !s.Updater.Newer(*rel) {
		s.mu.Unlock()
		return errors.New("there is no newer version to install")
	}
	if s.installed == rel.Version { // in place already; only the restart is missing
		s.mu.Unlock()
		if s.Restart != nil {
			s.Restart()
		}
		return nil
	}
	s.installing, s.err = true, ""
	s.mu.Unlock()
	s.changed()

	err := s.Updater.Install(ctx, *rel)
	s.mu.Lock()
	if err != nil {
		s.installing, s.err = false, "The update was not installed: "+err.Error()
	} else {
		s.installed = rel.Version // still "installing" until the restart has happened
	}
	s.mu.Unlock()
	if err != nil {
		s.changed()
		return err
	}
	if s.Restart != nil {
		s.Restart()
	}
	return nil
}

// Run checks shortly after start and then daily, for as long as the setting
// is not "off". In automatic mode it installs what it finds as soon as
// nothing is running.
func (s *Service) Run(ctx context.Context) {
	wait := firstCheck
	for first := true; ; first = false {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if first {
			s.Updater.Cleanup() // by now the program this one replaced has exited
		}
		wait = wakeEvery
		mode := s.Mode()
		if mode == Off {
			continue
		}
		st := s.Status()
		due := checkEvery
		if st.Error != "" {
			due = retryCheck
		}
		if st.Checked == nil || time.Since(*st.Checked) >= due {
			st = s.Check(ctx)
		}
		s.mu.Lock()
		failed, installed := s.failed, s.installed
		s.mu.Unlock()
		if mode != Auto || !st.Available || st.Blocked != "" || st.Latest.Version == failed || st.Latest.Version == installed {
			continue
		}
		if s.Idle != nil && !s.Idle() {
			wait = retryIdle
			continue
		}
		if s.Install(ctx) != nil {
			s.mu.Lock()
			s.failed = st.Latest.Version
			s.mu.Unlock()
		}
	}
}
