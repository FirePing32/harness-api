package transport

import "time"

// Timeout is how long a transport operation may take.
var Timeout = 30 * time.Second

func Name() string { return "transport" }
