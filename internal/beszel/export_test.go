package beszel

import "time"

// SetNow replaces the clock c reads token expiry against.
func SetNow(c *Client, now func() time.Time) { c.now = now }
