# Fallback regulatory database

`regulatory.db` and `regulatory.db.p7s` are the upstream wireless-regdb
release **master-2023-09-01**, unmodified, from
<https://git.kernel.org/pub/scm/linux/kernel/git/wens/wireless-regdb.git>.
`LICENSE` is upstream's (ISC).

| File | SHA-256 |
|---|---|
| regulatory.db | 0a4abd7ae20d07bb70642937ccb2293a72a6504730eea45a698882599f586368 |
| regulatory.db.p7s | bcd81aed039ea6b9b6f3726fbf26911a0caf4a5d894210e0fa2effb384d6b326 |
| sforshee.x509.pem | 9b3d2882637f0ee23e91761875bdbb40c7e38def01d73e28888e864d3c6e6076 |

**Why it is here.** Kernels built with `CFG80211_REQUIRE_SIGNED_REGDB` only
load a `regulatory.db` signed by a key compiled into them. From 2024 the
database is signed by its current maintainer (Chen-Yu Tsai, "wens"), whose
certificate entered the kernel in later 6.1.y stable releases. Older kernels,
including Libre Computer's 6.1.54, reject current distribution databases and
stay in the world domain `00`, where most 5 GHz channels are passive-only
and no mesh can start. master-2023-09-01 is the last release signed by the
previous maintainer (Seth Forshee), whose key those kernels do trust.

Verified with:

```sh
openssl cms -verify -binary -inform DER -in regulatory.db.p7s \
  -content regulatory.db -certfile sforshee.x509.pem -noverify -purpose any -out /dev/null
```

`scripts/install-regdb-fallback.sh` installs it only when the running kernel
rejects the installed database. The long-term path is the Trellis-signed
database for `unrestricted` mode (architecture §5.1), or a newer kernel.
