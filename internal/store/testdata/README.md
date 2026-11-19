# Store test data

- `schema19.db`: a database made by hataori at schema version 19, the release
  before thread numbers. The daemon ran with the fake surface and the fake
  agent, under the instance root `/tmp/hataori-fixture/root`, with two
  channels, `c1` and `c2`: two threads were started in `c1` (the first got a
  second message) and one in `c2`. It holds their threads, sessions, turns,
  events, outbox rows, catch-up cursors, triggers and inbox. The database was
  copied with `VACUUM INTO` after the daemon stopped. Tests move the recorded
  paths from that root to their own.
- `schema20-unlinked.db`: a database made by hataori at schema version 20,
  the release with the migration to links but before the control API named
  threads by number. Its daemon ran the first startup step on a new
  database, under the same instance root, with one channel `c1`, and then
  started one thread there, whose link, events' thread number, outbox rows'
  link and workspace path it did not write. Copied as above.
