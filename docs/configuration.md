# Configuration

One YAML (or JSON) file, by default `matchstick.yaml`; `--config` names another. Unknown fields are errors. The file
holds no secrets. Examples: [examples/azure.yaml](../examples/azure.yaml), [examples/fake.yaml](../examples/fake.yaml).

## Top level

| Field | Default | Meaning |
|---|---|---|
| `instance` | required | names this installation: lower-case letters and digits, starting with a letter, at most 10. It starts every machine, runner and scale-set name and marks every machine; installations sharing a GitHub organisation or a resource group need different ones |
| `interval` | `30s` | the longest time between two rounds of a pool |
| `listen.metrics` | `:9090` | serves `/metrics` and `/healthz` |
| `listen.admin` | `127.0.0.1:9091` | serves the drain endpoint; keep it on a loopback address |
| `github` | | where the scale sets live; required unless `fake` is set |
| `azure` | | where the machines go; required unless `fake` is set |
| `fake` | | made-up pools instead of GitHub and Azure, to try the planner |
| `pools` | required | the pools |

## `github`

| Field | Default | Meaning |
|---|---|---|
| `url` | required | the organisation, repository or enterprise, such as `https://github.com/acme`, `https://github.com/acme/app` or `https://github.com/enterprises/acme`; GitHub Enterprise Server URLs work the same way |
| `runnerGroup` | `default` | the runner group of the scale sets |
| `app.clientID`, `app.installationID` | | the GitHub App to sign in as, with its private key in the secret mount; without `app`, a token from the secret mount |

The App needs, on an organisation: **Self-hosted runners: read and write** (organisation permission). On a repository:
**Administration: read and write**.

## `azure`

| Field | Meaning |
|---|---|
| `subscriptionID`, `resourceGroup`, `location` | where VMs and network interfaces are created |
| `subnetID` | the subnet's resource ID; machines get a private address there and no public one |

matchstick's identity needs, on the resource group: create, read and delete VMs and network interfaces; on the
subnet: join (`Microsoft.Network/virtualNetworks/subnets/join/action`); on the image: read; on each pool's identity:
assign (`Microsoft.ManagedIdentity/userAssignedIdentities/assign/action`). The built-in roles Virtual Machine
Contributor and Network Contributor on the resource group cover the group's part.

## `pools[]`

| Field | Default | Meaning |
|---|---|---|
| `name` | required | lower-case letters, digits and inner hyphens, starting with a letter, at most 20 |
| `labels` | the name | the scale set's labels, which workflows ask for in `runs-on` |
| `maximum` | required | the most machines at once, in any state; 0 keeps the pool empty |
| `spares` | 0 | idle machines kept ready beyond demand, at most `maximum` |
| `maxLifetime` | `6h` | a machine older than this is deleted, job or not |
| `registrationTimeout` | `10m` | a machine whose runner is not online by then is deleted; a registration without a machine for this long is removed |
| `azure.size` | required | the VM size, such as `Standard_D4ads_v5` |
| `azure.image` | required | a Compute Gallery image version's resource ID, or an image definition's for its latest version |
| `azure.identity` | none | a user-assigned managed identity's resource ID for the VMs |
| `azure.ephemeralOSDisk` | managed disk | `ResourceDisk`, `CacheDisk` or `NvmeDisk`: the OS disk on the VM's local storage; the size must have room for the image |
| `azure.osDiskSizeGB` | the image's | the OS disk's size |
| `azure.spot` | false | Spot priority (evicted VMs are deleted), regular priority for 30 minutes after Spot has no capacity |
| `azure.acceleratedNetworking` | false | accelerated networking on the network interface |
| `azure.customData` | none | a file whose contents become the VM's custom data, such as a cloud-init configuration |

## `fake`

`fake.pools.<name>` gives a pool's `demand`, `machines` (`name`, `state`, `createdAt`, `expiresAt`) and `runners`
(`id`, `name`, `createdAt`, `online`, `busy`). `plan` and `status` show them; `run` acts on them in memory.
