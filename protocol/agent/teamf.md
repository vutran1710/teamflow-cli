Teamflow: choose the project and topics this session works on, stop, status (the project at a glance), the
approval requests waiting for the person, approve or reject a signal, upgrade teamflow.

Arguments: project [<project> [topic …]] | stop | status | approvals [--all] | approve <sg_id> [note] | reject <sg_id> <note> | upgrade

Run `{TEAMFLOW} team --relay {ARGUMENTS}` with the person's arguments exactly as they typed them, and nothing else
for this command. Tell the person what it prints in one or two short lines. For status, then show the project block
it prints (from the project's name to its last line) exactly as printed, in a code block. If it lists approval
requests or projects to choose from, ask about them as it says.
