package conn

// DialNonBlock creates a non-blocking TCP connection to the given address.
// Returns the raw file descriptor set to O_NONBLOCK.
// On non-Linux platforms, this function returns an error.
