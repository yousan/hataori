// Package workerssh maintains main-initiated OpenSSH connections to already
// running worker residents. It forwards private Unix sockets, validates the
// registered worker protocol binding before readiness and never starts agents
// or services through SSH.
package workerssh
