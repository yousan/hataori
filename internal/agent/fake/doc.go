// Package fake is an agent for tests: an adapter, its codec, and a small
// program the session host runs. It imitates no real agent.
//
// # Forms
//
// On pipes (Config.Terminal false) the program writes one JSON object per
// line to stdout and reads one per line on stdin. In a terminal it reads
// pasted input from the terminal (a bracketed paste, then Enter; a lone
// Escape interrupts) and keys a person types there (echoed, then Enter: a
// turn "by" "terminal", whose script is the line), reports each event by running `hataori hook` with the
// object as the payload (`--wait` for a question, whose answer the hook
// prints), and writes its text to a transcript file that it names in its
// first report. It never writes anything a reader needs to its terminal;
// only the script command "screen" writes there, for a person to look at,
// and, each time its terminal takes a new size, it writes the line
// "fake: size COLSxROWS" there, as an agent redraws its screen.
//
// # Wire format
//
// Every report is a JSON object with a field "t":
//
//	{"t":"ready","session":S,"model":M,"effort":E,"resume":R,"transcript":P}
//	{"t":"turn","by":"caller"|"agent"|"terminal","input":I,"n":N,"prompt":P}   (N: its number since the program started; P, by terminal only: the line typed)
//	{"t":"text","text":T,"final":true|false}   (on a terminal: in the transcript, with "turn":N)
//	{"t":"tool","id":I,"name":N,"parent":P,"end":false|true}   (P: the subagent it runs in)
//	{"t":"subagent","id":I,"name":N,"end":false|true}
//	{"t":"ask","id":I,"text":Q,"choices":[C...],"multi":true} (on a terminal: a waiting hook call; choices only when it has some, multi only for askmany)
//	{"t":"file","path":P,"more":[P...],"caption":C}
//	{"t":"usage","in":N,"output":N,"cache_read":N,"cache_write":N,"context_used":N,"context_size":N,"model":M}
//	{"t":"limits","at":MS,"every":S,"windows":[{"name":N,"minutes":M,"used":U,"resets":R}...]}
//	{"t":"end","status":"completed"|"failed"|"interrupted","reason":R,"background":N}
//	{"t":"interrupted"}                          (transcript only)
//	{"t":"cannot-resume","session":S}            (then it exits)
//	{"t":"session-end","reason":R}               (in a person's window, --window: as it exits)
//	{"t":"mystery"}                              (a kind no codec knows)
//
// On pipes the program reads {"input":T}, {"answer":{"id":I,"text":A}} and
// {"interrupt":true}.
//
// An input with an Input.ID starts with the line "@input I"; the program
// takes it off the script and reports I in the turn's "input".
//
// # Scripts
//
// The input of a turn is its script, one command per line:
//
//	reply TEXT          say TEXT; the last reply is the final answer
//	whoami              reply "model M effort E", as the program was started
//	heard               reply "heard: L", L the first line of the input
//	tool NAME DURATION  run a tool (a child process) for DURATION
//	ask QUESTION [-- CHOICE | CHOICE...]
//	                    ask, with its choices if any, wait for the answer,
//	                    then reply "answer: A"; an answer with choices is
//	                    their tokens joined by ", " (any text is ignored);
//	                    the first " -- " starts the choices, which are
//	                    trimmed, and empty ones are dropped
//	askmany QUESTION -- CHOICE | CHOICE...
//	                    ask, as ask does, a question whose choices may be
//	                    chosen several at once
//	deaf DURATION       run a tool named deaf for DURATION without heeding an
//	                    interrupt; one that came is heeded at its end
//	background DURATION end the turn with one background task, then start a
//	                    turn by itself after DURATION
//	fail REASON         end the turn as failed with REASON
//	exit                exit at once, without ending the turn
//	interrupt-delay D   from now on, an interrupt takes effect D after it
//	                    comes, as an agent that is slow to stop
//	session-end [REASON] in a person's window (--window): report the session's
//	                    end, for REASON (prompt_input_exit by default), and go
//	                    on, as a hook can before its process ends; "clear" is
//	                    a /clear, after which an agent goes on
//	unknown             report a line of a kind no codec knows
//	screen TEXT         write TEXT and a line end to the terminal; nothing on pipes
//	subagent ID [NAME [TOOL...]]
//	                    report a subagent's start, its tools, and its end
//	file PATH [PATH...] [-- CAPTION]
//	                    hand over files, with a caption
//	usage IN OUT CACHE_READ CACHE_WRITE CONTEXT_USED CONTEXT_SIZE [MODEL]
//	                    report the turn's tokens so far and the context use
//	limits AGE EVERY [NAME:MINUTES:USED:RESETS...]
//	                    report the account's remaining usage as given AGE
//	                    seconds ago, reported every EVERY seconds; RESETS is
//	                    Unix seconds, 0 for none
//	mcp TOOL [ARGUMENTS]
//	                    call TOOL of the session's MCP server, ARGUMENTS a JSON
//	                    object, and reply "mcp: TEXT", "mcp error: TEXT" for a
//	                    tool error, or "mcp failed: ..." when it was not reached
//	write NAME TEXT     write TEXT and a line end to the file NAME, relative to
//	                    its working directory, making the directories it is
//	                    in, as an agent's file tool writes; reply "wrote NAME"
//	                    or "write error: ..."
//	read NAME           reply "read: TEXT", TEXT the content of the file NAME,
//	                    relative to its working directory, without its last
//	                    line end, or "read error: ..."
//
// With --mcp-config FILE (a confined spec, Config.Confines) the program has
// the MCP servers in FILE. It starts the first one on the first "mcp" and
// keeps it until it exits, as an agent keeps its MCP servers.
//
// With --conversations DIR the program keeps one file per session it starts
// in DIR, and a resume whose file is gone reports "cannot-resume".
//
// With --config DIR every turn fails with the reason login until DIR holds
// the file LoggedInFile, which the login command writes:
//
//	login --config DIR --accept CODE [--no-link] [--quit]
//
// prints a link line and a request for a code, refuses any other code and
// asks again, and logs in on CODE. --no-link prints nothing and waits for
// its input to end; --quit ends cleanly at the first code without logging
// in.
//
// With --report-model M and --report-effort E (Config.ReportModel and
// ReportEffort) the program reports M and E when it is ready, instead of
// the model and effort it was started with; whoami still replies with the
// ones it was started with.
//
// The spec's instructions are the start-up script: a line "never-start"
// makes the program never report that it is ready, "input-delay D"
// makes it wait D before it takes each input, and "relay A B" makes it a
// supervisor that relays between threads A and B: each turn whose input
// has a line starting "Thread A:", as a wake message does, ends by
// sending "reply relayed from A" to thread B with the MCP server's
// send_to_thread, and the other way round; a send refused as busy is
// tried again for up to 10 seconds. A line "wake-script" makes it run, for
// a wake message (input that starts "hataori: "), the lines of that
// message quoted with "> " as the turn's script, without their "> ".
//
// The codec holds input while a turn runs and writes it when the turn
// ends, as an agent that takes one prompt at a time would. Written input
// whose turn is not reported within Config.InputTimeout ends as failed,
// "not delivered".
package fake
