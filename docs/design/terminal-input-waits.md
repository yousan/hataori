# Refused terminal input and Stop

A terminal can refuse Enter or an interrupt while its pane is in copy mode,
another process is in the foreground, or its host cannot be reached. Bytes
that were not handed over do not prove that the editor is empty. A pending
Enter must not be discarded so the next message can be pasted behind it.

After an owed Enter has waited for the input timeout, the Claude Code
adapter reports one `input_waiting` event for that input. The turn stays
open, Enter retries continue, and the chat thread shows why the message is
waiting. Retries do not post repeated notices. A refused Stop button replies
to the button's message and notifies the person who pressed it; the button
remains usable. Repeated deliveries of the same refusal are deduplicated per
person and turn. Other people can receive their own refusal notice.

An owed Enter or an explicitly refused interrupt does not start the Stop
watchdog, even for an agent hataori started and even after the input timeout.
A Stop has not reached the agent, so it is reported as not interrupted. A
person can leave the pane's blocking mode and retry, or explicitly close or
reset the thread to end an owned agent. Hataori does not clear the editor or
escalate this refusal to killing it automatically.

For an owned agent, failure to contact its host while trying to interrupt a
running turn keeps the existing Stop watchdog: after its grace period it
attempts to stop the owned session, provided the same turn is still open.
This does not apply to an agent taken in from a person's terminal. An owned
agent whose Enter is owed still follows the refusal policy above, regardless
of whether that handoff failed because of a pane or an unreachable host.
The waiting notice describes the known fact, that Enter could not be handed
to the window; it does not guess which terminal mode caused it.

These choices preserve explicit ownership and input boundaries. A transport
failure or an old screen alone is no permission to edit a person's text.
