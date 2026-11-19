# Telegram

Thicket runs on Telegram, Discord, or both at once. Telegram uses long
polling, so it needs no public address or open port.

## Setting up the bot

1. In Telegram, message [@BotFather](https://t.me/BotFather), send
   `/newbot`, and pick a name and username. BotFather replies with a token
   like `123456789:AA…`.
2. Set `TELEGRAM_TOKEN` to it (in `docker-compose.yml`, or as a stack
   variable in Portainer) and start Thicket.
3. Open a chat with your bot and press **Start**. Juni says hello and asks
   for an OpenRouter key, as on Discord.

If Thicket has no owner yet, the first person to message the bot becomes
the owner, unless `THICKET_OWNER_TELEGRAM_ID` names someone. Your numeric
id is shown by bots such as @userinfobot.

## Using both Discord and Telegram

Each app account starts out separate. To talk to the same sprigs, with
the same memories, from both:

1. On the app Thicket already knows you on, ask Juni to link your other
   app ("I want to use Telegram too").
2. Thicket DMs you a one-time code such as `LINK-7KQ2-M9XD`. Juni never
   sees it.
3. Send just the code to the bot in a private chat on the other app,
   within 15 minutes.

Proactive messages (job results, follow-ups, check-ins, alerts) go to
the app you last wrote from, preferring Telegram or Discord over the web
app when you have both. To choose, tell a sprig "reach me on Telegram"
(or "Pip should write on Discord" for one sprig; "auto" goes back to the
default). If sending there fails, your other linked apps are tried
before the message waits in the inbox.

An account that already has its own memories, jobs or owner role can't be
folded into another profile. Codes are single use, and a few wrong codes
in an hour make Thicket stop answering that account's guesses.

Inviting someone else works on either app: "invite Telegram user
123456789". Telegram bots can't look people up by @username, so use their
numeric id.

## How it looks

A Telegram bot posts under one name and picture, so each reply starts with
the speaking sprig's name in bold. Sprigs' Markdown is converted to
Telegram's formatting (bold, code, links, headings); if Telegram refuses
a message's formatting, it's sent as plain text.

Photos, PDFs and text files work as on Discord. Telegram limits bots to
downloading files up to 20 MB. Voice messages and audio are transcribed
(`transcribe_model`); video arrives by name only. The photos of an album
are gathered into one message.

A voice message gets a voice note back, after the text reply, in the
sprig's own voice (see [Speaking out loud](characters.md#speaking-out-loud)).
Thicket asks the speech server for Ogg Opus, which Telegram shows as a
voice note (OpenRouter only makes MP3, so with it you get an audio file
Telegram plays inline). If a server sends another format, or you've
blocked voice messages from bots in Telegram's privacy settings, it
arrives as a file instead. "Delete that" deletes the spoken copy along
with the text. Send `/voice brief`, `/voice full` or `/voice off` to choose
how the sprig you're talking to answers out loud.

When a message looks like a key or password, Thicket deletes it. Telegram
lets bots do this in private chats, unlike Discord.

## Groups

Add the bot to a group, and sprigs answer when you @-mention the bot,
reply to one of its messages, or use a command such as `/start`.

By default Telegram's privacy mode only shows bots those messages. To
address sprigs by name ("Pip, what's on today?") or to have them answer
everything in a group, turn privacy mode off: in @BotFather, use
`/setprivacy`, then remove and re-add the bot to the group. List those
group ids in `THICKET_TELEGRAM_CHATS` to have sprigs answer every message
there. Group ids are negative numbers, such as `-1001234567890`.

## Reactions

React with ✍ on any message to have it remembered for good, as if you'd
replied "remember this" (Telegram's reactions don't include 🧠). Thicket
asks Telegram for reaction updates only for this. In a private chat they
always arrive; in a group, Telegram tells bots about reactions only when
the bot is an administrator there.

## Settings

| Variable | |
|---|---|
| `TELEGRAM_TOKEN` | bot token from @BotFather |
| `THICKET_OWNER_TELEGRAM_ID` | your numeric Telegram id (optional) |
| `THICKET_TELEGRAM_CHATS` | comma-separated group ids where sprigs answer everything |

`THICKET_ALLOWED_USERS` accepts plain ids, or `telegram:ID` and
`discord:ID` to be exact.
