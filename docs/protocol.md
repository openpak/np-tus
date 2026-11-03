# Protocol

Firmware 4.93. VSH performs every TUS request on behalf of the game.

1. `GET http://static-resource.np.community.playstation.net/np/resource/title/<commid>/storage/<commid>-storage.xml`
   returns `<npstorage status="alive" titleid="<commid>"><storage-fqdn>HOST</storage-fqdn></npstorage>`,
   preceded by the `<!--Sce-Np-Signature: …-->` HMAC-SHA1 line (`np-common/resource`), keyed with the
   storage resource key. The console caches it for 60 seconds.
2. `POST https://HOST/user_storage/sec/<op>` with an XML body rooted at `<npstorage platform="ps3" sv="4.93">`
   carrying `<ticket>` (base64), `<ticketjid>`, `<psid>`, `<npcommid>`, `<sign>` and the op's elements.
   Slot numbers and values are decimal attributes; JIDs, sizes, base64 info and RFC 3339 dates are element text.
3. Replies are HTTP 200 with a `Content-Length` and `<npstorage result="XX">`; `00` is success, any other
   two-hex value is the error the game sees (`02` bad ticket, `05` unknown user, `06` forbidden, `4a` no data,
   `73` condition not met). Lists are `[<hits>N</hits>]<list>…</list>`.
4. `set_data` and a successful `get_data` carry raw bytes after the XML: XML, `\r\n\r\n`, data, with
   `Sce-Np-Info-Length` giving the XML length.
5. TSS is a plain `GET https://a0.ww.np.dl.playstation.net/tss/np/<commid>/<commid>-<slot>.tss`; the body is
   handed to the game as is. Any status other than 200, 206 or 304 reads as zero bytes.

The request `sign` element and `Sce-Np-Anticheat` header are HMACs with the title's passphrase; they are not
checked. `tryset_variable` stores the value when `value <try> current` holds (`comp-value` replaces the
compared operand); that operand order is inferred, not observed.

## Storage

The operations above are translated to the `title-storage` core (`docs/protocol.md` there): variable
ops to `SetVariables` / `ListVariables` / `AddVariable` / `TrySetVariable` / `DeleteVariables`, data
ops to objects in collection `tus` named by the decimal slot, written public. Core error to result:
`PermissionDenied` is `06`, `ResourceExhausted` (size cap, quota) `1a`, `InvalidArgument` `ff`,
anything else `07`. Times are stored at 10 ms resolution, so a printed `date` echoes back as `comp-date`.
