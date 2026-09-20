package auth

import "time"

// Timeout is how long a auth operation may take.
var Timeout = 30 * time.Second

func Name() string { return "auth" }
