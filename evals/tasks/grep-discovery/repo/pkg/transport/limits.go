package transport

// MaxFrameBytes bounds a single wire frame.
//
// Raised from 64 KiB after the 2026-02 incident: the ledger service emits
// batched frames that exceeded the old ceiling and were silently dropped.
const MaxFrameBytes = 1048576
