# The runner image's contract

matchstick creates each VM from the pool's image and hands it one thing: GitHub's just-in-time (JIT) runner
configuration, a single-use credential that registers one runner for one job. The image does the rest.

## What the image must do

1. **Read the JIT configuration** from the instance metadata on boot. On Azure it is the VM's user data, base64
   encoded:

   ```sh
   curl -fsS -H Metadata:true \
     "http://169.254.169.254/metadata/instance/compute/userData?api-version=2021-01-01&format=text" | base64 -d
   ```

2. **Run GitHub's runner once** with it, as an unprivileged user: `./run.sh --jitconfig "$jit"`. The runner registers,
   takes one job, and exits; GitHub then removes its registration.
3. **Power off when the runner exits**, whatever its exit status, and also when step 1 or 2 fails. matchstick deletes
   a stopped VM on its next round.

## Timing

- The runner must be online within the pool's `registrationTimeout` (default 10 minutes) of the VM's creation, or
  matchstick deletes the VM and its registration.
- A VM older than the pool's `maxLifetime` is deleted whatever it is doing.

## Versions

matchstick creates scale sets with runner self-update off, so the image ships the runner it runs. GitHub stops
accepting runner versions some time after a newer one is released; keep the runner in the image current.

## What the image gets

- The pool's managed identity, if `azure.identity` is set; none otherwise.
- A private address in the configured subnet and no public address; outbound access is whatever the network allows.
  The runner needs HTTPS to GitHub.
- An unusable SSH key for the user `runner` (Azure requires one); there is no way to log in.
- The pool's `azure.customData` file, if set, as custom data (cloud-init reads it).

## Security notes

- Any process on the VM can read the JIT configuration from the instance metadata until the runner has used it; it
  is single-use and registers only this runner.
- A job's code can use the VM's managed identity; give pools that run untrusted code no identity.

## Example

[examples/cloud-init.yaml](../examples/cloud-init.yaml) meets the contract on an Ubuntu image: it installs a pinned
runner release (checksum verified), a script that reads the JIT configuration and runs the runner once, and a
systemd unit that starts it at boot; the script powers the VM off when it ends. Use it as `azure.customData`, or bake
the same files into the image.
