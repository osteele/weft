// Package jobview contains read-only job classification helpers shared by
// terminal UI, narrate, and other presentation surfaces.
//
// Classifiers in this package use DB state as the source of truth. They do not
// repair rows or apply display-only heuristic overrides.
package jobview
