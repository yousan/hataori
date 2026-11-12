// Package fake is an agent for tests: an adapter, its codec, and a small
// program the session host runs. It imitates no real agent.
//
// # Forms
//
// On pipes (Config.Terminal false) the program writes one JSON object per
// line to stdout and reads one per line on stdin. In a terminal it reads
// pasted input from the terminal (a bracketed paste, then Enter; a lone
// Escape interrupts), reports each event by running `hataori hook` with the
// object as the payload (`--wait` for a question, whose answer the hook
// prints), and writes its text to a transcript file that it names in its
// first report. It never writes anything a reader needs to its terminal.
//
// # Wire format
//
// Every report is a JSON object with a field "t":
//
//	{"t":"ready","session":S,"model":M,"effort":E,"resume":R,"transcript":P}
//	{"t":"turn","by":"caller"|"agent"}
//	{"t":"text","text":T,"final":true|false}   (on a terminal: in the transcript)
//	{"t":"tool","id":I,"name":N,"end":false|true}
//	{"t":"subagent","id":I,"end":false|true}
//	{"t":"ask","id":I,"text":Q}                 (on a terminal: a waiting hook call)
//	{"t":"file","path":P}
//	{"t":"end","status":"completed"|"failed"|"interrupted","reason":R,"background":N}
//	{"t":"interrupted"}                          (transcript only)
//	{"t":"mystery"}                              (a kind no codec knows)
//
// On pipes the program reads {"input":T}, {"answer":{"id":I,"text":A}} and
// {"interrupt":true}.
//
// # Scripts
//
// The input of a turn is its script, one command per line:
//
//	reply TEXT          say TEXT; the last reply is the final answer
//	tool NAME DURATION  run a tool (a child process) for DURATION
//	ask QUESTION        ask, wait for the answer, then reply "answer: A"
//	background DURATION end the turn with one background task, then start a
//	                    turn by itself after DURATION
//	fail REASON         end the turn as failed with REASON
//	exit                exit at once, without ending the turn
//	unknown             report a line of a kind no codec knows
//	subagent ID         report a subagent's start and end
//	file PATH           hand over a file
//
// The spec's instructions are the start-up script: a line "never-start"
// makes the program never report that it is ready.
package fake
