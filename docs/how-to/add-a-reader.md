# Add a reader

Give somebody else an account on this archive, without learning their password.

Accounts are invite-only. There is no sign-up page, and there will not be: this is
a household archive, not a service.

## What a reader gets

Their own subscriptions, categories, tags, highlights, and reading state. They
cannot see yours — not your feeds, not what you have saved, not even whether an
article you have kept exists.

What they share with you is **the archive itself**: the articles, the extracted
bodies, and the stored images. One page is fetched once and kept once, however
many people subscribe to the feed that carried it.

## 1. Create the account

From the interface, as an administrator: **Accounts → Add someone**.

Or from the command line, which is also what to use when nobody can sign in:

```console
$ tome user add jane
created "jane" as reader, id 2
it has no password yet; `tome user link jane` issues one they can set themselves
```

Add `--admin` if they should also manage domain rules, retention and accounts. Most
people should not: it is about changing what everyone shares, not about what
anybody may read.

A new account **has no password and cannot be signed in to.** That is deliberate —
it means nobody, including you, has ever known it.

## 2. Hand over a setup link

Press **Issue link** beside their name, or:

```console
$ tome user link jane --base-url https://tomekeeper.example
https://tomekeeper.example/set-password?token=Kd9…
usable once, until 2026-08-29T12:00:00-05:00
```

Send it however you like. Whoever opens it chooses the password, so it never
passes through you.

Three things worth knowing:

- **It is shown once.** Only a hash is stored, so it cannot be looked up again.
- **It works once**, and expires after a week.
- **Issuing another stops the first one working.** If a link goes astray, issue a
  replacement rather than hoping.

They open the link, choose a password, and sign in with it.

## Resetting a forgotten password

The same link does it. Press **Reset password** beside their name, or run
`tome user link` again. It supersedes anything outstanding.

Setting a password — however it is set — **signs that reader out of every browser
and disconnects their mobile clients**, because the Fever API key is derived from
the password. That is unavoidable rather than a choice: the client computes the key
from what the reader types.

If handing over a URL is not possible, `tome user passwd jane` sets one directly.
It reads from standard input and does **not** hide what you type, so prefer the
link.

## Changing your own name

**Settings → Your name.** It asks for your password, and that is not only because a
rename changes how you sign in: mobile clients authenticate with a key derived from
your name *and* your password, so they will need reconnecting afterwards. The key
cannot be recomputed without the password, which is why the form asks.

## Changing your own password

**Settings → Password.** It asks for your current one, then signs out your *other*
browsers and keeps the one you are using.

**Settings → Signed-in devices** ends every session including the current one, for
when you have signed in somewhere you no longer control.

## Leaving

**Settings → Leaving.** A reader can delete their own account without asking
anybody: their subscriptions, tags, highlights and reading state go, and every
article and image stays. It asks for the password, because a signed-in browser
somebody walked away from should not be enough to destroy their reading.

The last administrator cannot leave — an archive with none cannot make another — so
make somebody else one first.

## Removing a reader

**Accounts → Delete**, which asks first, or `tome user rm jane`.

It removes their subscriptions, tags, highlights and everything they had read or
saved. **Every article and image is kept** — the archive belongs to the household,
not to one reader.

What that leaves is articles nothing references any more. Nothing collects them
automatically; [`tome prune`](../reference/cli.md#tome-prune) reports what they are
worth before removing anything.

**The last administrator cannot be deleted or demoted.** An archive with no
administrator cannot make one through the interface, and the way back would be a
hand-written `UPDATE` against the database.
