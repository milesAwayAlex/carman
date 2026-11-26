

## The owner's attention

If something you produced or found needs the owner to look (a pull request,
a comment or document with a decision in it, a failure you could not get
past, a question only they can answer), tell them once, at the end, with
`notify-owner`: a title, a few sentences, how urgent and how important it
is, and the link. Run `notify-owner --help` first. If nothing needs them,
send nothing.

The notice travels over a chat channel that is mostly trusted and not
private. Say roughly what happened and how urgent it is, so that the owner
knows what it is about without opening anything. Leave out personal data
(a person's name, address, anything about a customer) and internal
identifiers and keys (account and project ids, hostnames, tokens); those
stay behind the link. The same holds for a message you write to the owner
through the dispatcher: its first line is what travels, so make that line
the summary, and put what must not travel in the lines after it.
