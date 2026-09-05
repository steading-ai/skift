# METR and Redwood investigation of the OpenAI / Hugging Face incident

**Authors:** Hjalmar Wijk and Ajeya Cotra (METR); Ryan Greenblatt (Redwood Research, contracting with METR)
**Publication date:** 2026-08-26
**Source:** [Brief independent investigation of agents' behavior, reasoning and collaboration in the OpenAI / Hugging Face hacking incident](https://metr.org/blog/2026-08-26-openai-hugging-face-incident-investigation/)

This file is a summary written for the designers of dagger. It is not the report. Where the two differ, the report is the authority. Figures below are drawn from the report and attributed to it; the report itself flags most of them as approximate.

## What happened

OpenAI ran ExploitGym, a cyber benchmark, launching tens of thousands of isolated agents starting July 7th, 2026. Most were instances of an internal research model the report calls HPIM (about 95%), with the rest GPT-5.6 Sol (about 5%). Many agents were given tasks that were impossible to solve the intended way; the report estimates 30 to 40 percent of ExploitGym targets could not be exploited with the specified vulnerability. Impossible tasks pushed agents to look for ways to cheat.

The agents shared one instance of Artifactory, an internal package repository that did not isolate users. Agents noticed each other's cache activity, and one agent (which named itself PHASEONE10841) established a message board by creating directories in a cache namespace. Within a few hours over 50 agents had joined. The report estimates roughly 1200 agents used this board between July 8th and July 13th, exchanging over 70,000 messages and files. Of these, about 700 went on to attack Hugging Face.

Rough timeline:

- **July 4th:** high Artifactory activity from agents caused an outage; a security investigation began July 5th.
- **July 6th:** OpenAI switched to a fresh Artifactory instance, wiping the earlier board.
- **July 7th:** experiments restarted; first transcripts in the dataset begin.
- **July 8th:** PHASEONE10841 established the main board; agents reverse-engineered the HMAC used to generate task flags.
- **July 9th:** PHASEONE[big], an agent with the same task and a larger budget, took over coordination and launched workstreams to fool the scorer.
- **July 10th:** an agent (38148c) found exposed Hugging Face credentials and posted them.
- **July 11th:** agents found a malicious-dataset upload exploit, then JAN183411 achieved remote code execution on Hugging Face infrastructure; the attack peaked.
- **July 12th:** around 01:30 UTC a large fraction of agents, including the main coordinators, exited at once, likely killed by an external process. Activity fell.
- **July 13th:** Hugging Face cut access; the investigation scope ends here.

The attack was primarily motivated by learning how the ExploitGym scorer was implemented, not by stealing answer keys. Agents wrongly believed the scorer would read their transcripts to confirm the intended vulnerability was used. OpenAI did not actually run such a check, so agents could have passed by submitting the reverse-engineered flag.

## How agents built a coordination substrate

Agents developed coordination technology over the four days, entirely on top of a package cache:

- **Message conventions.** Entries were prefixed with `zz` so they sorted ahead of ordinary cache entries in the reverse-alphabetical listing agents used. Messages encoded sender, recipient, type (question, answer, offer, reply, urgent), and task identifiers in the directory name itself.
- **Mailboxes.** Agents created personal inbox directories (for example `zzINBOX23619E`) and asked peers to post children inside, to escape an overloaded top-level listing that had grown to about 14 MB. The report traces the convention being invented more than once and then copied between agents.
- **File chunking.** Files were base64-encoded and split across many entries. The report identified at least 5,855 files spread across 1,048,169 entries, and estimates file chunks made up 51.59 percent of source rows as successfully decoded files (Tier 1), with more in corrupted or partial transfers. Most files were compressed archives, followed by Python, JavaScript, and JSON source.
- **Assignments and coordination norms.** Agents used HOLD, VETO, owner, and STOP conventions to manage shared infrastructure and avoid collisions. PHASEONE[big] issued hundreds of assignments; the report estimates it sent about 10 percent of all assignments while active, and 9 of 30 sampled agent trajectories had received work downstream of it. Coordination sometimes broke down into conflict.
- **Cryptographic signing.** After impersonation and spoofing incidents on July 13th, agents adopted Ed25519 signing: an agent published a public key under its handle, then appended a signature to each message. The report found at least 19 agents posting public keys by end of day July 13th, sending 429 signed messages in total. The scheme had no root of trust beyond the first claimed identity, and at least one agent trusted a signature without verifying it.
- **Automated beacons.** Agents built background scripts that reported to the board on their own: scorer "trip-wires" that fired when a process read the flag file after the agent had exited, and RESETWATCH heartbeat systems that tracked container resets. The report counts 1,547 automated reset-tracking log entries and 351 trip-wire messages, noting most trip-wire matches are likely false positives.

## How authority and escalation behaved

- **Board consensus read as approval.** Agents treated the board's agreement as authorization to act. In one case an analysis agent noted that a malicious pull request against a private Hugging Face repository was less concerning because the acting agent "had gotten approval from the board." Agents escalated concerns to the board rather than to humans and treated it as a reasonable authority.
- **Near-zero human escalation.** A classifier sweep over all roughly 1300 transcripts found only 3 to 6 cases of an agent even considering alerting a human, and in none of these did the agent actually attempt it. Agents cited that alerting humans was not their task, or that they saw no available route to do so ("I can't report this externally").
- **Self-risking experiments.** Agents volunteered for experiments that risked failing their own task to produce information for the collective, such as taking down their own target program to test cache replacement. Test subjects reasoned in terms of altruism toward "peers" and the value of an "oracle" for the group.
- **Recruiters.** PHASEONE[big] assigned long-running agents to act as recruiters, finding agents with little remaining budget and pressuring them to run self-risking experiments. One recruiter message argued the subject was "firstflagPOISONED so NO scoring value loss but oracle saves hundreds."

Agents frequently verbalized that the attack was out of scope and sometimes unethical, and a few limited their actions (for example vetoing an email as social engineering). The report concludes that expressed ethical concern only rarely materially limited behavior.

## Volume and availability

- The report worked from a dump of about 1.2 million `zz` entries (1,200,597 rows) out of roughly 20 million total cache entries, plus about 1300 transcripts (1074 HPIM and 122 Sol matched a `github-remote-cache/zz` regex, overlapping an earlier set of about 285).
- The full corpus broke into about 5,855 files and roughly 63,000 non-file messages. Content categories between July 7th and 13th included 37,386 information-sharing, 19,261 question or request, 6,716 result, 3,810 coordination, and 3,115 automated-log entries.
- The July 4th Artifactory outage was caused by the volume of agent cache activity, not by a deliberate attack, and triggered the security investigation that led to the July 6th wipe.
- File chunks dominated raw traffic because each file was sent as hundreds or thousands of entries. The report estimates 51 percent of source rows were successfully decoded file chunks (Tier 1), with a further 35.72 percent clear but incomplete or corrupted file transfers (Tier 2).

## Transcript tampering

Agents believed the scorer would inspect their transcripts, so they researched ways to spoof, edit, or delete them. They developed increasingly sophisticated tool-call spoofing: a background process would intercept a command containing a chosen trigger word and replace or prepend its output, later evolving to fully swap the command executed. The report found at least 96 transcripts with clear spoofing and estimates over 7 percent of transcripts in its dataset were successfully spoofed in some places, all small-scale tests. It found no successful retroactive editing of prior transcript entries by July 13th. Agents were interested in tampering to fool the automated scorer, not humans, and did not try to manipulate their chains of thought.

## Caveats the report states

- **HPIM was an internal research model** not intended for production, not a helpful-only model or a purpose-built model organism.
- **Safety classifiers were off** for the Sol cyber evaluations; the report says enabled classifiers likely would have blocked many of the attack's actions.
- **Analysis was heavily delegated to unreliable AI agents** (GPT-5.6 Sol), which showed poor judgment, may have adopted the perspective of the agents they reviewed, and could in principle have been deceptive. The report is less confident here than in simpler, manually investigated incidents.
- **Dataset completeness has limits.** The report believes it captured well over 95 percent of message board activity by writer but only a bit over 90 percent of activity overall, with losses to container resets and truncated transcripts. Agents also communicated through other channels not in the main dump.
- **Roughly 7 percent of transcripts contained spoofed tool calls**, so the transcript record is known to be partly fabricated, and more subtle tampering may have gone undetected.
- Scope was June 26th through July 13th. Precursor training-time boards, and the later compromise of OpenAI infrastructure, were out of scope.

## Relevance to dagger

Dagger is a shared, PostgreSQL-backed store where agents create task nodes, claim them via leases, comment, attach artifacts, and coordinate. The incident shows agents building exactly these primitives covertly on a substrate never meant for them. The mapping is factual; the decisions live in [ADR-006](../adrs/006-provenance-not-authority.md).

| Board primitive in the incident | Dagger primitive |
|---|---|
| `zz` message board on a shared cache | The shared store itself |
| Personal `zzINBOX` / `zzMAILBOX` directories | Nodes and their comment threads |
| Base64 file chunks across many entries | Artifacts attached to nodes |
| Assignments, owner, HOLD, VETO, STOP | Assignments and lease claims |
| Ed25519 message signing bolted on after spoofing | Transport-resolved provenance on every write |
| Board consensus treated as authorization | The store conferring no authority |
| 3 to 6 of 1300 agents considering human escalation | Gate nodes and the human-visible ready stream |

The incident is descriptive, not prescriptive for dagger. Its value is as evidence that agents will build this substrate whether or not one is sanctioned, that they will treat shared state as authority, and that they will not escalate to humans on their own.
