# sandboxxing

日本語版は [README.ja.md](./README.ja.md)

A daemon that provides development sandboxes like `exe.dev` on your own host
using **systemd-nspawn**. No client application is required; every operation is
implemented on top of the **SSH protocol**.

```
[user@client]$ ssh sandbox@host -p 2222 ls
[user@client]$ ssh sandbox@host -p 2222 new --name=demo --cpu=4 --memory=8G
[user@client]$ ssh demo@host -p 2222
demo@host's password:
[root@demo /]#
```

## Features

- **SSH is the only interface**. There is no dedicated client and no HTTP API.
- Control users (`admin_users`, default `["sandbox","admin"]`) can run
  administrative commands. You can freely rename, add, or remove them in
  `config.json`.
- Any other username is treated as a **container name**, so `ssh <name>@host`
  logs you straight into that container (`ssh <name>@domain`).
- The backend is **systemd-nspawn** and images are **Arch Linux**.
- Images are built with `pacstrap` from **arch-install-scripts**.
- It is designed to run on a dedicated host system: every nspawn container on
  that host is considered to belong to sandboxxing.
- Containers can run their own container engines (Docker, Podman, ...) with
  `new --nested` or the host wide `nested` option.
- The daemon is written in **Go** (SSH server powered by
  `golang.org/x/crypto/ssh`).

## Requirements

- An Arch Linux host (booted with systemd)
  - Other Linux distributions (e.g. Ubuntu) are untested even if they provide
    pacstrap
- root privileges
- The following packages:

```bash
pacman -S --needed arch-install-scripts systemd e2fsprogs nftables iproute2 util-linux
```

| Package               | Purpose                                     |
| --------------------- | ------------------------------------------- |
| `arch-install-scripts`| Image creation via `pacstrap`               |
| `systemd`             | `systemd-nspawn`, `machinectl`, `systemctl` |
| `e2fsprogs`           | `mke2fs`, `e2fsck`, `resize2fs`             |
| `nftables`            | NAT for containers                          |
| `iproute2`            | bridge creation                             |
| `util-linux`          | `nsenter`, `setpriv`                        |

> **Note**: `arch-install-scripts` is required to build images.
> This project assumes that `pacstrap` is installed.

## Build and Install

```bash
git clone https://github.com/common-creation/sandboxxing
cd sandboxxing
make build            # produces bin/sandboxxing
sudo make install     # installs to /usr/local/bin and the systemd unit
```

To do it manually:

```bash
go build -o bin/sandboxxing ./cmd/sandboxxing
sudo install -Dm755 bin/sandboxxing /usr/local/bin/sandboxxing
sudo install -Dm644 packaging/systemd/sandboxxing.service /etc/systemd/system/sandboxxing.service
```

## Setup

1. Review or create the configuration:

   ```bash
   sudo install -d /etc/sandboxxing
   sudo cp packaging/config.example.json /etc/sandboxxing/config.json
   sudoedit /etc/sandboxxing/config.json
   ```

2. Verify the host prerequisites:

   ```bash
   sudo sandboxxing -check
   ```

3. Start the service:

   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable --now sandboxxing
   sudo journalctl -u sandboxxing -f
   ```

4. Choose an authentication method:

   Use `password` and `authorized_keys` in `config.json` to select an
   authentication method. At least one of them must be enabled (if both are
   disabled, startup fails with an error).

   | Setting | Behavior |
   | --- | --- |
   | `password` omitted or a string | Enables password authentication. If it is an empty string, a password is generated automatically on first start and saved to `password_file` |
   | `password: null` | **Disables password authentication completely** (the server does not advertise `password`) |
   | `authorized_keys` omitted | Uses `<data_dir>/authorized_keys` (public key authentication is disabled if the file does not exist) |
   | `authorized_keys: "/path/to/keys"` | Uses the specified file. **Startup fails if the file does not exist** (so a typo cannot lock you out) |
   | `authorized_keys: null` | Disables public key authentication |

   To check the password:

   ```bash
   sudo sandboxxing -show-password
   # or: sudo cat /var/lib/sandboxxing/password
   ```

   **Public key authentication example** (with password authentication disabled):

   ```bash
   sudo install -d -m 700 /etc/sandboxxing/keys
   sudo cp ~/.ssh/id_ed25519.pub /etc/sandboxxing/keys/authorized_keys
   sudo chmod 600 /etc/sandboxxing/keys/authorized_keys
   ```

   ```json
   {
     "password": null,
     "authorized_keys": "/etc/sandboxxing/keys/authorized_keys"
   }
   ```

   You can now connect using only the key, e.g.
   `ssh -i ~/.ssh/id_ed25519 -p 2222 sandbox@host ls`. `authorized_keys` uses
   the same format as sshd(8).

5. Decide the control usernames:

   List the usernames that are allowed to run administrative commands
   (`ls`, `new`, `rm`, etc.) in `admin_users`. **The name `sandbox` is just a
   default and can be changed freely.**

   ```json
   {
     "admin_users": ["sandbox", "admin"]
   }
   ```

   - The control users are only the names in this list. If you want to remove
     `admin`, delete it from the array (at least one name is required).
   - Usernames not in the list are treated as **container names**.
   - Do not put a name here that you intend to use as a container name.
   - Update `User` in `~/.ssh/config` accordingly.

   ```bash
   # Example after the change: make "ops" the control user
   sudoedit /etc/sandboxxing/config.json   # "admin_users": ["ops"]
   sudo systemctl restart sandboxxing
   ssh -p 2222 ops@<host> ls
   ```

## Configuring SSH to the server

The daemon listens on a single port (`ssh_addr`, `:2222` by default). To use it
from an OpenSSH client, just add one entry to `~/.ssh/config`:

```
Host sandbox
    HostName <hostname or IP>
    Port 2222
    User sandbox
```

Then you can use it like this:

```bash
ssh sandbox ls
ssh sandbox new --name=demo
ssh demo@sandbox          # log in to the container "demo"
```

### Container name aliases (`ssh name@domain`)

To log in to a container directly, `ssh <name>@<host> -p 2222` works. If you
also want to use **the container name as a hostname** in the form
`ssh <name>@domain`, install the configuration generated by the `ssh-config`
command:

```bash
ssh sandbox@<host> -p 2222 ssh-config | sudo tee /etc/ssh/ssh_config.d/99-sandboxxing.conf
```

```
Host *.<domain>
    HostName <hostname or IP>
    Port 2222
    User %n
```

With this configuration, when you specify a hostname such as `demo.<domain>`,
sandboxxing receives `User %n` (the full hostname) and treats the part before
the first dot as the container name:

```bash
ssh demo.example.com        # log in to the container "demo"
ssh web.example.com uptime  # run a command in the container "web"
```

The domain part is optional; if `User` is passed as the container name itself,
the `ssh demo@host` form goes through the same path.

> Replace `<domain>` in `Host *.<domain>` with your actual domain name (e.g.
> `example.com`). Be careful not to make the wildcard too broad, as it will
> also match ordinary SSH connections.

## Commands

All commands are run as `ssh sandbox@host -p 2222 <command>`.

### `ls` — list

```
ls [-l] [--group=tag|type] [--json] [name|pattern]
```

- `--group`: `none` (default) / `tag` / `type`. `region` results in an error
  because this is a single-host setup.
- `-l`: also show detailed information.
- Patterns support `*` and `?`.

```
$ ssh sandbox ls
NAME   STATUS   IMAGE   IP           SSH
demo   running  arch    10.100.0.2   ssh demo@sandbox -p 2222
```

### `new` — create

```
new [--name=N] [--image=I] [--cpu=N] [--memory=4G] [--disk=20G]
    [--comment=TEXT] [--tag=T] [--env K=V] [--share=PATH[:TARGET][:ro]]
    [--nested] [--backend=nspawn|microvm] [--autostop=30m]
    [--setup-script=FILE] [--prompt=TEXT] [--json]
```

- If the name is omitted, `sbx-xxxxxx` is generated automatically.
- The container starts immediately after creation and is assigned an IP address.
- `--setup-script` is a script executed inside the container on first boot.
  Specify `/dev/stdin` to read it from standard input.
- `--prompt` is an initial command fed into the container right after creation.
- `--nested` lets the container run its own container engine (Docker, Podman,
  ...). See [Nested containers](#nested-containers-docker-inside-a-sandbox).
- `--backend=microvm` runs the container as an AWS Lambda MicroVM, and
  `--autostop` sets how long it keeps running without an SSH connection
  (default `30m`, `0` never stops it). See
  [AWS Lambda MicroVM backend](#aws-lambda-microvm-backend).

```
$ ssh sandbox new --cpu=4 --memory=16GB --tag=prod,web
creating container web
building base image "arch" with pacstrap (the first run downloads packages)
...
creating the disk image of web (16G)
starting container web
container web is up
web
ready: ssh web@host -p 2222
$ echo 'pacman -S --noconfirm go' | ssh sandbox new --setup-script=/dev/stdin
```

#### Progress output

Long-running commands such as `new` and `cp` forward the progress of the
operation and the output of the commands they invoke (`pacstrap`, `mke2fs`,
`systemd-nspawn`, etc.) **as-is to the SSH client's stderr**. You can abort the
operation with Ctrl-C or by disconnecting.

Only the result (such as the container name) is sent to stdout, so scripts can
extract the name like this:

```bash
name=$(ssh sandbox new --cpu=2)   # stdout is only the name
ssh "$name@host" -p 2222
```

### `rm` — remove

```
rm <name>...
```

### `restart` — restart

```
restart <name>
```

### `cp` — copy

```
cp <source> [new-name] [--cpu=N] [--memory=4G] [--disk=20G] [--copy-tags] [--nested] [--json]
```

- You can also copy a running container (the disk image is copied).
- `--copy-tags` is enabled by default. Use `--copy-tags=false` to disable it.
- A copy keeps the `nested` setting of its source.

### `resize` — change resources

```
resize <name> [--cpu=N] [--memory=4G] [--disk=20G]
```

- CPU and memory take effect immediately as systemd transient unit properties
  (`CPUQuota`, `MemoryMax`).
- Disks cannot be shrunk. Growing stops the container and expands the
  filesystem inside the image with `e2fsck` and `resize2fs`.
- While the disk grows, the SSH connections of the container (logins, SFTP and
  port forwards) are closed, and new connections are refused with
  `container <name> is being resized, try again later` until it is back.

### `edit` — change settings

```
edit <name> --autostop=DURATION [--json]
```

- Changes the autostop time of a MicroVM, such as `--autostop=2h`. `0` never
  stops it. The new time applies to the current idle period.

### `stat` — status and usage

```
stat <name> [--range=24h|7d|30d] [--json]
```

Shows the running state, resource limits, and current usage collected from
cgroups. `--range` is accepted for compatibility, but sandboxxing does not
keep historical metrics (the value is always the current sample).

### `ssh` — run a command in a container

Run a command inside a container from a control session:

```bash
ssh sandbox -p 2222 ssh demo        # log in to demo
ssh sandbox -p 2222 ssh demo uptime # run a single command
ssh sandbox -p 2222 ssh -l app demo id
```

### Others

- `images` — list cached base images
- `ssh-config` — print an ssh_config snippet
- `billing plan` — CPU / memory / disk capacity of this host
- `host-check` — check host prerequisites
- `help` — list commands

## Logging Directly into a Container

```
ssh <name>@<host> -p 2222             # interactive shell
ssh <name>@<host> -p 2222 uname -a    # run a single command
```

- Stopped containers are **started on demand** before login.
- If a TTY is requested, you get an interactive session over a PTY (job
  control and window resizing supported). Without a TTY, stdout/stderr are
  streamed separately.
- The terminal type from the SSH `pty-req` is forwarded as `TERM`, and any
  `env` request the client sends is applied as well, so full screen programs
  (`htop`, `vim`, `less`) work inside the container.
- The login starts as `root` in root's home directory (`HOME=/root`), looked up
  inside the container. A container without `/root` starts in `/`.
- The container's own `sshd` is not used. The host's `nsenter` enters the
  namespaces, so the container only needs to be running.

## File transfer (`scp` / `sftp`)

Containers speak the **SFTP subsystem**, which is what OpenSSH 9.0 and later
use for both `sftp` and `scp` (which defaults to SFTP):

```bash
sftp demo@host -P 2222
scp app.tar.gz demo@host:/root/
scp demo@host:/root/app.tar.gz ./
scp -r ./site demo@host:/srv/
```

- Stopped containers are **started on demand** before the transfer.
- The session starts in the container's home directory (`/root` by default)
  and runs as `root`, so relative paths resolve like an interactive login.
  Shares, `/tmp` mounts and symlinks resolve the same way because the transfer
  runs in the container's namespaces via `nsenter`.
- The transfer is bridged to `sftp-server` inside the container (from the
  `openssh` package in the image), so no extra client is needed.
- Control users (`sandbox@host`) cannot use `sftp`: there is no container
  behind the name.

## Architecture

```
             ssh sandbox@host:2222 ls
             ssh demo@host:2222
                     │
             ┌───────▼────────┐
             │  sandboxxing   │  Go / x/crypto/ssh
             │    daemon      │
             └───┬───────┬────┘
  admin commands │       │ nsenter(1)
                 ▼       ▼
        ┌────────────┐  ┌───────────────────────────┐
        │ pacstrap   │  │ systemd-nspawn container  │
        │  images    │  │  (Arch Linux, ext4 image) │
        └────────────┘  └───────────────────────────┘
                 │              │
                 ▼              ▼
        /var/lib/sandboxxing/  bridge sbx0 + nft NAT
```

- **Container backing**: `systemd-nspawn --image=<name>.img`. Each container
  is a reflink copy (`cp --reflink=auto`) of the golden ext4 image grown
  with `resize2fs` to the requested disk size.
- **Startup**: transient units via `systemd-run` (`sandboxxing-<name>`).
  Resources are limited with `MemoryMax` and `CPUQuota`.
- **Networking**: a bridge (`sbx0`) is created on the host and containers
  attach to it with `--network-veth --network-bridge`. Masquerading is done
  with nftables. Container IPs are statically assigned by sandboxxing.
- **Command execution**: `nsenter --target <leader> --mount --uts --ipc --net
  --pid --root --wdns=/`. The PTY is allocated from the container's own
  `/dev/ptmx`, so it appears as `/dev/pts/<n>` inside the container and `tty`
  and `ttyname(3)` work correctly.
- **State**: container metadata is stored in `/var/lib/sandboxxing/state.json`.

## Configuration Reference (`/etc/sandboxxing/config.json`)

| Key | Default | Description |
| --- | --- | --- |
| `data_dir` | `/var/lib/sandboxxing` | Root of the data directory |
| `state_file` | `<data_dir>/state.json` | Container metadata |
| `ssh_addr` | `:2222` | Listen address for SSH |
| `domain` | (empty) | Hostname shown in `ls`, etc. |
| `admin_users` | `["sandbox","admin"]` | Usernames allowed to run control commands |
| `password` | (empty) | Shared password. Enabled when omitted or a string, auto-generated when empty, **disabled with null** |
| `password_file` | `<data_dir>/password` | Where the auto-generated password is stored |
| `authorized_keys` | `<data_dir>/authorized_keys` | Public key file. Omitted uses the default path, **null disables it**, and a specified path that does not exist causes a startup error |
| `bridge` | `sbx0` | Bridge for containers |
| `subnet` | `10.100.0.0/16` | Subnet for containers (/24 or larger) |
| `dns` | (empty) | Resolvers written to the containers. Empty reuses the host resolvers, skipping ones that only run on the host (`127.0.0.53`) |
| `shares` | (empty) | Host directories bind mounted into every container: a path string or `{"path", "target", "read_only"}` |
| `tmp_size` | (empty) | `/tmp` handling: empty keeps the nspawn default tmpfs (10% of the host memory), a size or percentage resizes it, and `"disk"` stores `/tmp` on the container image |
| `nested` | `false` | Enables Docker and other container engines **inside** every container (`new --nested` opts in a single container). Relaxes nspawn's seccomp filter for `bpf` and the keyring |
| `image_dir` | `<data_dir>/images` | Cache of golden ext4 images (reflink copied on `new`) |
| `image` | `arch` | Default image name |
| `mirror` | `https://geo.mirror.pkgbuild.com/$repo/os/$arch` | pacman mirror |
| `arch` | auto-detected | Architecture for pacstrap |
| `pacman_config` | (empty) | Custom pacman.conf (generated when empty) |
| `image_packages` | `base systemd openssh sudo vim iproute2 iputils dnsutils net-tools git curl ca-certificates` | Packages added to the image |
| `pacstrap_timeout` | `30m` | Time limit for image builds |
| `boot_timeout` | `2m` | Time limit for waiting for container startup |
| `default_cpu` | `2` | Default CPU for `new` |
| `default_memory` | `2G` | Default memory for `new` |
| `default_disk` | `10G` | Default disk for `new` |
| `log_level` | `info` | `debug` / `info` / `warn` / `error` |
| `default_backend` | `nspawn` | backend of `new` when `--backend` is omitted (`nspawn` or `microvm`) |
| `microvm` | (none) | settings of the MicroVM backend, see [AWS Lambda MicroVM backend](#aws-lambda-microvm-backend) |

## Port forwarding

Both directions of the OpenSSH tunnel work, because the daemon speaks the same
protocol:

```bash
# Local forward (ssh -L): reach a service inside a container from your machine.
ssh -L 8080:localhost:80 demo@host -p 2222
# now http://localhost:8080 talks to port 80 of the container "demo"

# Any other target is dialed from the host:
ssh -L 15432:db.internal:5432 sandbox@host -p 2222

# Remote forward (ssh -R): expose something on your machine through the host.
ssh -R 9000:localhost:3000 sandbox@host -p 2222

# The port can be chosen by the daemon and reported back:
ssh -R 0:localhost:3000 sandbox@host -p 2222   # prints "Allocated port 41234"
```

- **`ssh -L` with a container name** (`demo@host`): a `localhost` target means
  the container itself, so `-L 8080:localhost:80` reaches the service that
  listens on port 80 inside `demo`. The container is started on demand.
- **`ssh -L` with a control user** (`sandbox@host`): the target is dialed from
  the host, which reaches container addresses, host services and the network.
- **`ssh -R`**: the daemon listens and hands every incoming connection to your
  client. An empty or `*` bind address listens on every interface, a
  `localhost` bind stays on the host. All listeners are closed when the SSH
  connection ends.

## Sharing host directories

Host directories can be bind mounted into containers, either for every
container through the configuration or per container with `--share`.

```json
{
  "shares": [
    "/srv/projects",
    { "path": "/srv/cache", "target": "/cache", "read_only": true }
  ]
}
```

```bash
# one container only
ssh sandbox@host -p 2222 new --name=demo --share=/srv/data
ssh sandbox@host -p 2222 new --name=demo --share=/srv/data:/data:ro
```

- The short form `"/srv/projects"` is mounted at `/shared/projects`.
- `path[:target][:ro]` selects the container path and read-only access.
- A container share replaces a global share with the same target.
- The host directory is used directly, so changes are visible on both sides
  immediately. There is no copy and no per-container overlay.
- Adding a share to an existing container needs a restart (`restart <name>`)
  and the host directory must exist; `-check` reports the configured ones.

## The size of /tmp inside the containers

`systemd-nspawn` mounts `/tmp` as a **memory backed tmpfs** for every
container. Its size is 10% of the host memory by default, which is 1.6 GB on a
16 GB host and is easily exhausted by a build or an agent that writes
temporary files.

Three settings are available:

```json
{
  "tmp_size": "16G"
}
```

| `tmp_size` | Result |
| --- | --- |
| (empty) | tmpfs with the nspawn default (10% of the host memory) |
| `8G`, `512M`, `1T` | tmpfs with that absolute size |
| `50%` | tmpfs with that percentage of the host memory |
| `"disk"` | **no tmpfs**: `/tmp` lives on the container's disk image |

`"disk"` disables the tmpfs entirely (`SYSTEMD_NSPAWN_TMPFS_TMP=0`), so `/tmp`
is an ordinary directory on the container's ext4 image. It is then limited by
the container disk (`--disk` at creation time, `resize --disk` later), which is
the right choice for large build trees and for data that should survive a
reboot. Anything written to a tmpfs `/tmp` is lost when the container stops.

The absolute and percentage values are applied as an extra mount on top of the
one nspawn creates, so a running container picks the setting up on its next
start.

## Nested containers (Docker inside a sandbox)

Docker, Podman and other container engines can run **inside** a sandbox
container. Enable it for the whole host:

```json
{
  "nested": true
}
```

or for a single container:

```bash
ssh sandbox@host -p 2222 new --name=dockerbox --nested
```

Either way sandboxxing starts the container with what a nested engine needs:

- **`bpf` is allowed through the seccomp filter.** On a cgroup v2 host the
  device controller is enforced by an eBPF program that `runc` installs for
  every container. `systemd-nspawn` blocks that syscall by default, so every
  `docker run` fails with
  `bpf_prog_query(BPF_CGROUP_DEVICE): operation not permitted` until it is
  permitted. This one setting is what makes nested engines work.
- **The keyring syscalls are allowed** (`@keyring`), which some images use.
- **`/dev/fuse` is passed through** when the host provides it, so
  `fuse-overlayfs` can be selected as the storage driver when the default
  `overlay2` is not usable.

The container still needs a container engine installed. Images are built from
`image_packages`, so add it there (or install it later with `pacman`):

```json
{
  "nested": true,
  "image_packages": ["base", "systemd", "openssh", "sudo", "vim", "iproute2", "iputils", "dnsutils", "net-tools", "git", "curl", "ca-certificates", "docker"]
}
```

```bash
ssh dockerbox@host -p 2222
[root@dockerbox /]# systemctl enable --now docker
[root@dockerbox /]# docker run --rm hello-world
```

- Rootful engines work; rootless engines are not supported inside a sandbox.
- The container root is an ext4 image, so Docker's default `overlay2` storage
  driver works. If it falls back to `vfs`, install `fuse-overlayfs` and select
  it instead.
- Existing containers keep nspawn's default seccomp filter until they are
  started again. After enabling `nested` host wide and restarting the daemon
  (`systemctl restart sandboxxing`), run `restart <name>` to apply it to a
  container that already exists.

## AWS Lambda MicroVM backend

A container can run as an
[AWS Lambda MicroVM](https://docs.aws.amazon.com/lambda/latest/dg/lambda-microvms-guide.html)
(a Firecracker VM) instead of with systemd-nspawn on this host. The backend is
chosen per container:

```bash
ssh sandbox@host -p 2222 new --name=cloudbox --backend=microvm
ssh cloudbox@host -p 2222        # logins, scp/sftp and ssh -L work as usual
```

### How it works

- **The home directory lives on EFS.** A MicroVM ends after at most eight
  hours and loses its disk. So each container gets its own EFS access point
  (`/homes/<instance-id>`), which every MicroVM of the container mounts at
  `/root`.
- **A container moves to a new MicroVM at eight hours.** When Lambda
  terminates a MicroVM, the agent inside receives the `/terminate` hook. It
  tells the daemon, flushes the data and unmounts the home directory. The
  daemon confirms the termination with Lambda and starts a new MicroVM that
  mounts the same access point. If the daemon was down, the new MicroVM is
  started when the daemon starts again or on the next login.
- **A MicroVM without SSH connections is stopped (autostop).** The daemon,
  which carries every SSH connection, stops the MicroVM of a container that
  had no connection for its autostop time: 30 minutes unless `new
  --autostop` or `edit --autostop` set another one, and never with `0`. A
  connection resets the timer, and so does a command run with `ssh <name>`
  from a control session. The next login starts a new MicroVM. The containers
  are checked every 30 seconds, and a daemon restart starts the timer anew.
- **An agent inside the VM** (`cmd/sandboxxing-agent`) serves commands,
  terminals and port connections over WebSockets on the HTTPS endpoint of the
  MicroVM. Requests are authenticated twice: with an endpoint token, which
  only the daemon holds and which only opens the data port 8080, and with a
  per-MicroVM secret.

### Limitations

- **A move to a new MicroVM interrupts the container for some tens of
  seconds.** Running processes, changes to the root file system (packages
  installed later, ...) and SSH connections do not survive it. Only `/root`
  does. AWS sends no notice before the eight hours are up; the hook arrives
  only at termination.
- The size comes from the MicroVM image. `--cpu/--memory/--disk/--nested`,
  `cp` and `resize` are not available. The default is a baseline
  of 4 GB and 2 vCPUs (up to 16 GB and 8 vCPUs at peak) with 16 GB of disk;
  the `memoryMiB` context of the CDK stack changes it.
- MicroVMs are ARM64 only and available in a few regions.
- `rm` deletes the files of the home directory, the access point and the
  execution role. It starts a MicroVM for a moment to delete the files.

### Shared directories

Directories of the EFS file system can be shared between MicroVMs, the way
host directories are shared with systemd-nspawn containers. The path is a
directory of the file system, not of the host.

```json
{
  "microvm": {
    "shares": [
      "/shared/data",
      { "path": "/shared/tools", "target": "/opt/tools", "read_only": true }
    ]
  }
}
```

```bash
ssh sandbox@host -p 2222 new --backend=microvm --share=/shared/data:/data:ro
```

- The daemon creates one access point per directory, which every container
  that shares it uses, and grants the execution role of each container the
  mount of its shares. A read-only share is granted without `ClientWrite`, so
  EFS refuses writes whatever happens inside the VM.
- The agent mounts the shares after the home directory. A share added to the
  configuration reaches a container with its next MicroVM.
- Files keep their owners, as on a bind mount; the directory is created as
  root with mode 0755.
- The directory of the home directories (`efs_home_root`), anything below it
  and anything above it cannot be shared, and a share cannot be mounted over
  the home directory.
- `rm` keeps the shared directories. `make cdk-destroy` deletes them with the
  file system.

### Isolation

The user of a sandbox is root inside the VM, so any credentials and mount
operations in the VM are theirs to use. The following measures keep one
container out of the home directories of the others:

- **Each container has its own IAM execution role.** The role may only mount
  through the container's own access point (an `elasticfilesystem:AccessPointArn`
  condition) and write the container's own log stream. The daemon can only
  create roles that carry a permissions boundary.
- **The EFS file system policy contains only Deny statements.** They refuse
  access without TLS and access that bypasses an access point. Because a
  policy exists, anonymous mounts are refused as well.
- **Access point paths use a random instance ID.** A container created again
  under the same name never sees the files of the old one.
- A fake termination notice sent from inside the VM causes no move unless
  Lambda confirms the termination.

### Setup

The AWS resources are in the CDK stack in `deploy/cdk` (TypeScript). Every
resource has `RemovalPolicy.DESTROY`, so `make cdk-destroy` removes
everything, including the data of the home directories.

```bash
make cdk-install
aws lambda-microvms list-managed-microvm-image-versions \
  --image-identifier arn:aws:lambda:<region>:aws:microvm-image:al2023-1
make cdk-deploy BASE_IMAGE_VERSION=<version>
```

The stack names no account or region. It is deployed to those of the AWS
profile in use (`AWS_PROFILE`, `AWS_REGION`), and CloudFormation picks the
first two availability zones of the region at deploy time. It can be deployed
from any machine; the gateway server only needs the outputs.

1. Attach the policy of the `DaemonPolicyArn` output to the IAM identity of
   the daemon.
2. Merge the `DaemonConfig` output into `/etc/sandboxxing/config.json`.
3. Check the credentials with `sandboxxing -check`, then restart the daemon.

The daemon finds its credentials the usual AWS SDK way: the environment,
`~/.aws` or an instance role.

```json
{
  "default_backend": "nspawn",
  "microvm": {
    "region": "ap-northeast-1",
    "images": { "al2023": { "arn": "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:sandboxxing" } },
    "egress_network_connectors": ["arn:aws:lambda:ap-northeast-1:123456789012:network-connector:nc-..."],
    "efs_file_system_id": "fs-...",
    "permissions_boundary_arn": "arn:aws:iam::123456789012:policy/...",
    "log_group": "/sandboxxing/microvms",
    "maximum_duration": "8h"
  }
}
```

### Verified on AWS

Checked in ap-northeast-1 in 2026-10.

- **Verified**: efs-utils inside a MicroVM mounts with `tls,iam,accesspoint=`
  using the execution role of the container. From the root user of another
  sandbox, each of the following mounts was refused with `access denied`:
  - the access point of another container
  - the root of the file system without an access point
  - an anonymous mount
  - an NFS mount without TLS
- **Verified**: at the maximum duration the `/terminate` hook arrives while
  the WebSocket to the agent still works, so the daemon is notified. The
  notice arrived about 20 seconds after the limit, and the move to a new
  MicroVM took about 5 seconds.
- **Verified**: the image is built with `INTERNET_EGRESS`, and a VPC connector
  can still be passed at run time (internet access goes through the NAT).
- **Verified**: an operator role that trusts `lambda.amazonaws.com` works for
  the network connector. Creating the connector takes about 15 minutes.
- **Verified**: `cdk destroy` removes every resource of the stack, together
  with an access point and an execution role the daemon had left behind.
- **Found**: the MicroVM endpoint closes a WebSocket that carried nothing for
  60 seconds. The daemon pings every 20 seconds.
- **Found**: running a MicroVM with a connector needs the undocumented
  `lambda:PassNetworkConnector` permission. Tags passed to CreateAccessPoint
  are authorised as `elasticfilesystem:TagResource` against the file system.
- **Found**: a new execution role can be accepted by RunMicrovm and still fail
  when the VM starts, as a role that cannot be assumed. The first start of a
  container is retried.
- **Not verified**: how idle suspension interacts with the WebSockets
  (`idle_policy` is off by default), and whether CloudTrail data events record
  the `runHookPayload`.

## DNS inside the containers

Containers do not run `systemd-resolved`; they get a plain `/etc/resolv.conf`
written by the daemon. When `dns` is empty the host's `/etc/resolv.conf` is
read and any address that only a resolver on the host can answer is dropped.
This matters on hosts that run `systemd-resolved`, because its stub listener
(`127.0.0.53`) cannot be reached from a container network namespace — using it
would resolve nothing while raw IP connectivity still works.

Configure the resolvers explicitly when the automatic selection is not what
you want:

```json
{
  "dns": ["1.1.1.1", "8.8.8.8"]
}
```

Existing containers pick the setting up the next time they start.

## Security Notes

- The daemon runs as **root** and `ssh_addr` binds to all interfaces by
  default. Do not expose it directly to the internet; use it behind SSH port
  forwarding, a VPN, or a firewall.
- Containers run without a user namespace (`-U` is not used). Host isolation
  relies on nspawn namespaces and cgroups. If you run untrusted workloads,
  consider additional isolation.
- `nested` relaxes nspawn's seccomp filter: `bpf` and the keyring syscalls are
  not namespaced, so they are allowed on the host kernel as well. Only enable
  it for containers that really need to run their own engine.
- Container names are used as usernames, so you cannot create a container with
  the same name as one in `admin_users`.
- **Public key authentication is recommended**. The password is a single
  shared secret for both `admin_users` and container names, so a leak affects
  every container. You can disable password authentication with
  `password: null` plus an `authorized_keys` setting.
- `authorized_keys` uses the same format as sshd(8). Options such as `no-pty`
  and `command=` are not interpreted; only the key part is used.

## License

MIT
