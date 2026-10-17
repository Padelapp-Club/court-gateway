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

// SetWSRetry sets the keep-alive's retry intervals for a test and returns
// what restores them.
func SetWSRetry(fast, slow time.Duration) (restore func()) {
	f, s := wsRetry, wsSlowRetry
	wsRetry, wsSlowRetry = fast, slow
	return func() { wsRetry, wsSlowRetry = f, s }
}
