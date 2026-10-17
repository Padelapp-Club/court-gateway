package lunda

import (
	"encoding/json"
	"time"
)

// EncodeSession is a saved session as the driver writes it, for tests that
// start logged in.
func EncodeSession(token, club, clubName string) []byte {
	raw, _ := json.Marshal(session{V: 1, Token: token, Club: club, ClubName: clubName})
	return raw
}

// SessionToken is the token of a saved session ("" when it does not read).
func SessionToken(raw []byte) string {
	var s session
	_ = json.Unmarshal(raw, &s)
	return s.Token
}

// SetWSRetry sets the keep-alive's retry intervals, and how long a
// connection must stay open to count as up, for a test and returns what
// restores them.
func SetWSRetry(fast, slow, stable time.Duration) (restore func()) {
	f, s, st := wsRetry, wsSlowRetry, wsStable
	wsRetry, wsSlowRetry, wsStable = fast, slow, stable
	return func() { wsRetry, wsSlowRetry, wsStable = f, s, st }
}
