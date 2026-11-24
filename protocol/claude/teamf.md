---
description: Teamflow — choose the project and topics this session works on, stop, status, your approval requests, approve or reject a signal
argument-hint: project [<project> [topic …]] | stop | status | approvals [--all] | approve <sg_id> [note] | reject <sg_id> <note>
allowed-tools: Bash({TEAMFLOW} team:*)
---
!`{TEAMFLOW} team --relay $ARGUMENTS`

The output above comes from Teamflow. Tell the user what it says in one or two short lines; if it lists approval requests or projects to choose from, ask about them as it says. Do not run anything else for this command, and never approve or reject a signal yourself.
