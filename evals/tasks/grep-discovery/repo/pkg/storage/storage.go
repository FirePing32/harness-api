package storage

import "time"

// Timeout is how long a storage operation may take.
var Timeout = 30 * time.Second

func Name() string { return "storage" }
