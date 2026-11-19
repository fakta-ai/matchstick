# The runner image's contract

matchstick creates each VM from the pool's image and hands it public bootstrap information only: where to claim its
runner configuration and which certificate to trust there. The configuration itself, GitHub's just-in-time (JIT)
runner configuration, a single-use credential that registers one runner for one job, never appears in the VM's
settings: the image claims it from matchstick, proving with a document Azure signs that it runs on that VM. The image
does the rest.

## What the image must do

1. **Read the bootstrap** from the instance metadata on boot: the VM's user data, base64-encoded JSON.

   ```sh
   curl -fsS -H Metadata:true --noproxy '*' \
     "http://169.254.169.254/metadata/instance/compute/userData?api-version=2021-02-01&format=text" | base64 -d
   ```

   | Field | Meaning |
   |---|---|
   | `machine` | this VM's name |
   | `claimURL` | where to post the claim |
   | `claimCertificate` | the claim endpoint's certificate (PEM); trust this one only, never the system's roots |

2. **Claim the runner configuration.** Fetch the attested document from the instance metadata and post it, as it
   is, to `claimURL` over HTTPS, trusting only `claimCertificate`:

   ```sh
   curl -fsS -H Metadata:true --noproxy '*' \
     "http://169.254.169.254/metadata/attested/document?api-version=2021-02-01"
   # {"encoding": "pkcs7", "signature": "..."}
   ```

   ```
   POST <claimURL>
   Content-Type: application/json

   {"machine": "<machine>", "proof": {"encoding": "pkcs7", "signature": "..."}}
   ```

   | Answer | Meaning | Do |
   |---|---|---|
   | 200 `{"jitConfig": "..."}` | the configuration, released once | go on to step 3 |
   | 403 `{"error": {"code": "refused", ...}}` | nothing will be released to this VM (matchstick restarted, or it was already claimed) | power off |
   | 400 | the request is malformed | power off |
   | 503, or no connection | the proof could not be checked now, or matchstick is unreachable | fetch a fresh document and try again, every few seconds, until the pool's `registrationTimeout` |
   | the server's certificate is not `claimCertificate` | matchstick restarted (its key is new), or another host answers | power off |

   Bypass any HTTP proxy for both the instance metadata and `claimURL`.

3. **Run GitHub's runner once** with it, as an unprivileged user: `./run.sh --jitconfig "$jit"`. The runner registers,
   takes one job, and exits; GitHub then removes its registration.
4. **Power off when the runner exits**, whatever its exit status, and also when step 1, 2 or 3 fails. matchstick
   deletes a stopped VM on its next round. Work the image does between the runner's exit and its power-off, such as
   sending the job's logs, has the time Timing gives it.

## Timing

- The runner must be online within the pool's `registrationTimeout` (default 10 minutes) of the VM's creation, or
  matchstick deletes the VM and its registration. matchstick refuses claims after that time.
- A VM whose runner's job has ended, which matchstick sees as its registration gone, is left to power off: matchstick
  deletes it at its next round once it stops, or once the pool's `registrationTimeout` has passed since matchstick
  first saw its registration gone, however long the job ran.
- A VM older than the pool's `maxLifetime` is deleted whatever it is doing.
- matchstick holds a VM's configuration in memory only. If it restarts before the VM has claimed, the VM's claim fails
  and the VM powers off; matchstick deletes it and creates a replacement if jobs need one.

## Versions

matchstick creates scale sets with runner self-update off, so the image ships the runner it runs. GitHub stops
accepting runner versions some time after a newer one is released; keep the runner in the image current.

## What the image gets

- The pool's managed identity, if `azure.identity` is set; none otherwise.
- A private address in the configured subnet and no public address; outbound access is whatever the network allows.
  The runner needs HTTPS to GitHub, and the VM must reach `claimURL`.
- An unusable SSH key for the user `runner` (Azure requires one); there is no way to log in.
- The pool's `azure.customData` file, if set, as custom data (cloud-init reads it).

## Security notes

- The bootstrap is public. Anyone allowed to read the VM in Azure can read it; it holds nothing that registers a
  runner.
- Any process on the VM can fetch the attested document and claim before the runner does. Run nothing before the
  claim but the image's own boot code. Once the configuration is released, matchstick releases nothing more for that
  VM, so a job cannot register a second runner.
- The runner's configuration is single-use and registers only this runner.
- A job's code can use the VM's managed identity; give pools that run untrusted code no identity.

## Example

[examples/cloud-init.yaml](../examples/cloud-init.yaml) meets the contract on an Ubuntu image: it installs a pinned
runner release (checksum verified), a Python script that claims the configuration as above, a script that runs the
runner once with it, and a systemd unit that starts it at boot; the script powers the VM off when it ends. Use it as
`azure.customData`, or bake the same files into the image.
