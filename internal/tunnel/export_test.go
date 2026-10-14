package tunnel

// Refused counts the requests and configs refused because the hub did
// not sign them for this gateway (heartbeats report it).
func (c *Client) Refused() int64 { return c.refused.Load() }
