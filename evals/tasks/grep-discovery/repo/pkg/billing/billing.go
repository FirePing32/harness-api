package billing

import "time"

// Timeout is how long a billing operation may take.
var Timeout = 30 * time.Second

func Name() string { return "billing" }
