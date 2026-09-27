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
    [--comment=TEXT] [--tag=T] [--env K=V] [--setup-script=FILE]
    [--prompt=TEXT] [--json]
```

- If the name is omitted, `sbx-xxxxxx` is generated automatically.
- The container starts immediately after creation and is assigned an IP address.
- `--setup-script` is a script executed inside the container on first boot.
  Specify `/dev/stdin` to read it from standard input.
- `--prompt` is an initial command fed into the container right after creation.

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
cp <source> [new-name] [--cpu=N] [--memory=4G] [--disk=20G] [--copy-tags] [--json]
```

- You can also copy a running container (the disk image is copied).
- `--copy-tags` is enabled by default. Use `--copy-tags=false` to disable it.

### `resize` — change resources

```
resize <name> [--cpu=N] [--memory=4G] [--disk=20G]
```

- CPU and memory take effect immediately as systemd transient unit properties
  (`CPUQuota`, `MemoryMax`).
- Disks cannot be shrunk. Growing stops the container and expands the
  filesystem inside the image with `e2fsck` and `resize2fs`.

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
- The container's own `sshd` is not used. The host's `nsenter` enters the
  namespaces, so the container only needs to be running.

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

- **Container backing**: `systemd-nspawn --image=<name>.img`. Disk images are
  created from the rootfs built by `pacstrap` using `mke2fs -d`.
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
| `image_dir` | `<data_dir>/images` | Cache of pacstrap trees (converted to ext4 images on `new`) |
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

## Security Notes

- The daemon runs as **root** and `ssh_addr` binds to all interfaces by
  default. Do not expose it directly to the internet; use it behind SSH port
  forwarding, a VPN, or a firewall.
- Containers run without a user namespace (`-U` is not used). Host isolation
  relies on nspawn namespaces and cgroups. If you run untrusted workloads,
  consider additional isolation.
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
