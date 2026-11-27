# Unconfirmed terminal input

A terminal handoff confirms bytes, not a prompt. A paste handed over in
part, a lost Enter, and an input restored as `Written` after a reconnect can
leave text in the editor after the turn's input timeout. Appending the next
message would submit both texts together. A person's text may be there too.

The Claude Code adapter therefore refuses later messages with `not delivered`
while any previous paste remains unconfirmed. It sends no further paste,
Enter, clearing key or idle interrupt into that editor. An unrelated turn
(including a background task notification) does not prove that the editor
was consumed. This also applies to normally encoded pastes restored after a
reconnect: the stored `Written` flag cannot establish whether Enter arrived.

The adapter keeps each such input pending when its turn fails. Its internal
`TurnEnded` event retains the input ID for attribution and sets `RetainInput`;
the runner keeps the initial input or pending row and its exact pasted text.
A rebuilt codec receives `Written` again and preserves the same protection.
Other adapters' events default to completing the input as before. This flag
is internal to the agent interface; it changes no control message or public
event encoding.

A matching `UserPromptSubmit` confirms consumption and releases only that
input. A CLI `SessionEnd` confirms that the old editor is gone, so remaining
unconfirmed inputs complete as not delivered. A person can submit or clear
the CLI conversation at the terminal, or end its process explicitly; hataori
does not guess from an old screen snapshot or clear their text on its own.
Clearing only the editor without a matching hook leaves the protection in
place, because the adapter has no fresh, reliable observation of that action.

The regressions replay the existing `first_enter_lost` screen capture and
`held_notification` hooks. No new editing keys or live CLI capture are needed.
