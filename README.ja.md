# sandboxxing

`exe.dev` のような開発用サンドボックスを、自分のホスト上で **systemd-nspawn**
を使って提供するデーモンです。クライアントアプリは不要で、操作はすべて
**SSH プロトコル**の上に実装されています。

```
[user@client]$ ssh sandbox@host -p 2222 ls
[user@client]$ ssh sandbox@host -p 2222 new --name=demo --cpu=4 --memory=8G
[user@client]$ ssh demo@host -p 2222
demo@host's password:
[root@demo /]#
```

## 特徴

- **SSH がすべてのインタフェース**。専用クライアントも HTTP API もない。
- 制御ユーザー (`admin_users`、既定 `["sandbox","admin"]`) は管理コマンドを
  実行できる。名前は `config.json` で自由に変更・追加・削除できる。
- それ以外のユーザー名は **コンテナ名**として扱われ、`ssh <name>@host` が
  そのままコンテナへのログインになる(`ssh <name>@domain`)。
- バックエンドは **systemd-nspawn**、イメージは **Arch Linux**。
- イメージのビルドに **arch-install-scripts** の `pacstrap` を使用する。
- 独立したホストシステムでの運用を前提とし、そのホスト上の nspawn
  コンテナはすべて sandboxxing のものとして扱う。
- コンテナ自身がコンテナエンジン(Docker, Podman など)を動かせる。
  `new --nested` か、ホスト全体向けの `nested` 設定で有効化する。
- デーモンは **Go** で実装(`golang.org/x/crypto/ssh` による SSH サーバー)。

## 必要な環境

- Arch Linux ホスト(systemd で起動していること)
  - その他のLinuxディストーション（ Ubuntu など）は pacstrap が提供されていても動作未確認
- root 権限
- 次のパッケージ:

```bash
pacman -S --needed arch-install-scripts systemd e2fsprogs nftables iproute2 util-linux
```

| パッケージ            | 用途                                    |
| --------------------- | --------------------------------------- |
| `arch-install-scripts`| `pacstrap` によるイメージ作成           |
| `systemd`             | `systemd-nspawn`, `machinectl`, `systemctl` |
| `e2fsprogs`           | `mke2fs`, `e2fsck`, `resize2fs`         |
| `nftables`            | コンテナ用の NAT                        |
| `iproute2`            | bridge の作成                           |
| `util-linux`          | `nsenter`, `setpriv`                    |

> **注意**: `arch-install-scripts` はイメージのビルドに必須です。
> 本プロジェクトは `pacstrap` がインストールされている前提で動作します。

## ビルドとインストール

```bash
git clone https://github.com/common-creation/sandboxxing
cd sandboxxing
make build            # bin/sandboxxing ができる
sudo make install     # /usr/local/bin と systemd unit を配置
```

手動で行う場合:

```bash
go build -o bin/sandboxxing ./cmd/sandboxxing
sudo install -Dm755 bin/sandboxxing /usr/local/bin/sandboxxing
sudo install -Dm644 packaging/systemd/sandboxxing.service /etc/systemd/system/sandboxxing.service
```

## セットアップ

1. 設定を確認する / 作る:

   ```bash
   sudo install -d /etc/sandboxxing
   sudo cp packaging/config.example.json /etc/sandboxxing/config.json
   sudoedit /etc/sandboxxing/config.json
   ```

2. ホストの前提を検証する:

   ```bash
   sudo sandboxxing -check
   ```

3. サービスを開始する:

   ```bash
   sudo systemctl daemon-reload
   sudo systemctl enable --now sandboxxing
   sudo journalctl -u sandboxxing -f
   ```

4. 認証方法を決める:

   `config.json` の `password` と `authorized_keys` で認証方法を選びます。
   どちらか一方は必ず有効にしてください(両方無効は起動時にエラーになります)。

   | 設定 | 動作 |
   | --- | --- |
   | `password` 省略 または 文字列 | パスワード認証を有効化。空文字なら初回起動時に自動生成し `password_file` に保存 |
   | `password: null` | **パスワード認証を完全に無効化**(サーバーが `password` を広告しない) |
   | `authorized_keys` 省略 | `<data_dir>/authorized_keys` を使用(ファイルが無ければ公開鍵認証は無効) |
   | `authorized_keys: "/path/to/keys"` | 指定したファイルを使用。**ファイルが無ければ起動エラー**(typo で締め出されないように) |
   | `authorized_keys: null` | 公開鍵認証を無効化 |

   パスワードを確認する:

   ```bash
   sudo sandboxxing -show-password
   # あるいは: sudo cat /var/lib/sandboxxing/password
   ```

   **公開鍵認証の例**(パスワード認証を無効にする場合):

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

   これで `ssh -i ~/.ssh/id_ed25519 -p 2222 sandbox@host ls` のように
   鍵だけで接続できます。`authorized_keys` は sshd(8) と同じ形式です。

5. 制御ユーザー名を決める:

   管理コマンド(`ls`, `new`, `rm` など)を実行できるユーザー名を
   `admin_users` に列挙します。**`sandbox` という名前は既定値にすぎず、
   自由に変更できます。**

   ```json
   {
     "admin_users": ["sandbox", "admin"]
   }
   ```

   - 制御ユーザーはこのリストの名前だけです。`admin` を消したい場合は
     配列から削除してください(1 つ以上の名前が必要です)。
   - リストに無いユーザー名は**コンテナ名**として扱われます。
   - コンテナ名として使う予定の名前をここに書かないでください。
   - `~/.ssh/config` の `User` も合わせて変更してください。

   ```bash
   # 変更後の例: 制御ユーザーを "ops" にする
   sudoedit /etc/sandboxxing/config.json   # "admin_users": ["ops"]
   sudo systemctl restart sandboxxing
   ssh -p 2222 ops@<host> ls
   ```

## サーバーへ SSH するための設定

デーモンは 1 つのポート(`ssh_addr`、既定では `:2222`)で待ち受けます。
OpenSSH クライアントから使うには、`~/.ssh/config` に 1 つエントリを足すだけです:

```
Host sandbox
    HostName <ホスト名または IP>
    Port 2222
    User sandbox
```

これで次のように使えます:

```bash
ssh sandbox ls
ssh sandbox new --name=demo
ssh demo@sandbox          # コンテナ demo にログイン
```

### コンテナ名のエイリアス (`ssh name@domain`)

コンテナへ直接ログインするには `ssh <name>@<host> -p 2222` が使えます。
さらに `ssh <name>@domain` の形で**コンテナ名をホスト名として**使いたい
場合は、`ssh-config` コマンドが生成する設定をインストールします:

```bash
ssh sandbox@<host> -p 2222 ssh-config | sudo tee /etc/ssh/ssh_config.d/99-sandboxxing.conf
```

```
Host *.<domain>
    HostName <ホスト名または IP>
    Port 2222
    User %n
```

この設定では `demo.<domain>` のようなホスト名を指定すると、sandboxxing が
`User %n`(ホスト名全体)を受け取り、最初のドットより前をコンテナ名として
扱います:

```bash
ssh demo.example.com        # コンテナ demo にログイン
ssh web.example.com uptime  # コンテナ web でコマンドを実行
```

ドメイン部分は任意で、`User` がコンテナ名のまま渡れば `ssh demo@host`
形式も同じ経路を通ります。

> `Host *.<domain>` の `<domain>` は実際のドメイン名(例: `example.com`)
> に置き換えてください。ワイルドカードを広くしすぎると、通常の SSH
> 接続にもマッチするため注意してください。

## コマンド

すべて `ssh sandbox@host -p 2222 <command>` の形で実行します。

### `ls` — 一覧

```
ls [-l] [--group=tag|type] [--json] [name|pattern]
```

- `--group`: `none`(既定) / `tag` / `type`。`region` は単一ホスト構成のため
  エラーになります。
- `-l`: 詳細情報も表示します。
- パターンは `*` と `?` が使えます。

```
$ ssh sandbox ls
NAME   STATUS   IMAGE   IP           SSH
demo   running  arch    10.100.0.2   ssh demo@sandbox -p 2222
```

### `new` — 作成

```
new [--name=N] [--image=I] [--cpu=N] [--memory=4G] [--disk=20G]
    [--comment=TEXT] [--tag=T] [--env K=V] [--share=PATH[:TARGET][:ro]]
    [--nested] [--backend=nspawn|microvm] [--autostop=30m]
    [--setup-script=FILE] [--prompt=TEXT] [--json]
```

- 名前を省略すると `sbx-xxxxxx` が自動生成されます。
- 作成後すぐに起動し、IP アドレスが割り当てられます。
- `--setup-script` は初回にコンテナ内で実行するスクリプトです。
  `/dev/stdin` を指定すると標準入力から読み込みます。
- `--prompt` は作成直後にコンテナへ流し込む初期コマンドです。
- `--nested` はそのコンテナ自身でコンテナエンジン(Docker, Podman など)を
  動かせるようにします。詳細は後述の「ネストしたコンテナ」を参照してください。
- `--backend=microvm` を指定すると、AWS Lambda MicroVM として動かします。
  `--autostop` は、SSH 接続がない状態で動かし続ける時間です(既定は `30m`。
  `0` で自動停止しません)。詳細は後述の「AWS Lambda MicroVM バックエンド」を
  参照してください。

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

#### 進捗出力

`new` や `cp` のように時間のかかるコマンドは、処理の進み具合と、その処理が
起動するコマンド(`pacstrap`, `mke2fs`, `systemd-nspawn` など)の出力を
**そのまま SSH クライアントの stderr へ転送**します。キャンセル(Ctrl-C)や
接続断で処理を中断できます。

処理結果(コンテナ名など)だけが stdout に届くため、スクリプトからは次の
ように名前を取り出せます:

```bash
name=$(ssh sandbox new --cpu=2)   # stdout は名前だけ
ssh "$name@host" -p 2222
```

### `rm` — 削除

```
rm <name>...
```

### `restart` — 再起動

```
restart <name>
```

### `cp` — 複製

```
cp <source> [new-name] [--cpu=N] [--memory=4G] [--disk=20G] [--copy-tags] [--nested] [--json]
```

- 実行中のコンテナを複製することもできます(ディスクイメージをコピー)。
- `--copy-tags` は既定で有効です。無効にする場合は `--copy-tags=false`。
- 複製したコンテナは元の `nested` 設定を引き継ぎます。

### `resize` — リソース変更

```
resize <name> [--cpu=N] [--memory=4G] [--disk=20G]
```

- CPU とメモリは systemd の transient unit プロパティ (`CPUQuota`,
  `MemoryMax`) として即時反映されます。
- ディスクは縮小できません。拡張はコンテナを停止し、`e2fsck` と
  `resize2fs` でイメージ内のファイルシステムを成長させます。
- ディスクの拡張中は、そのコンテナへの SSH 接続(ログイン、SFTP、ポート
  フォワード)を切断し、完了するまで新しい接続は
  `container <name> is being resized, try again later` で拒否します。

### `edit` — 設定変更

```
edit <name> --autostop=DURATION [--json]
```

- MicroVM の autostop 時間を変更します(例: `--autostop=2h`)。`0` を指定すると
  自動停止しません。新しい時間は、進行中の無接続時間にもそのまま適用されます。

### `stat` — 状態と使用量

```
stat <name> [--range=24h|7d|30d] [--json]
```

稼働状況、リソース上限、cgroup から取得した現在の使用量を表示します。
`--range` は互換性のために受け付けますが、sandboxxing は履歴メトリクスを
保持しません(値は常に現在のサンプルです)。

### `ssh` — コンテナ内でコマンド実行

制御セッションからコンテナ内のコマンドを実行します:

```bash
ssh sandbox -p 2222 ssh demo        # demo にログイン
ssh sandbox -p 2222 ssh demo uptime # 1 コマンドだけ実行
ssh sandbox -p 2222 ssh -l app demo id
```

### その他

- `images` — キャッシュ済みベースイメージの一覧
- `ssh-config` — ssh_config のスニペットを出力
- `billing plan` — このホストの CPU / メモリ / ディスクの容量
- `host-check` — ホストの前提条件チェック
- `help` — コマンド一覧

## コンテナ内への直接ログイン

```
ssh <name>@<host> -p 2222             # 対話シェル
ssh <name>@<host> -p 2222 uname -a    # 1 コマンド実行
```

- 停止しているコンテナは**オンデマンドで起動**してからログインします。
- TTY を要求した場合は PTY 経由で対話できます（ジョブ制御・ウィンドウ
  サイズ変更対応）。TTY がない場合は
  stdout/stderr が分離されたままストリームされます。
- SSH の `pty-req` で通知される端末種別を `TERM` としてコンテナへ渡し、
  クライアントが送る `env` リクエストも反映します。そのため `htop` や
  `vim`、`less` などの全画面アプリが動作します。
- ログインは **`root`** として root のホームディレクトリで開始します
  (`HOME=/root`)。ホームはコンテナ内で解決するため、コンテナ側の構成に
  従います。`/root` が無いコンテナは `/` で開始します。
- コンテナ内の `sshd` は使いません。ホスト側の `nsenter` で
  namespaces に入るため、コンテナは起動していれば十分です。

## ファイル転送 (`scp` / `sftp`)

コンテナは **SFTP サブシステム**を話します。OpenSSH 9.0 以降の `sftp` と
`scp`(既定で SFTP 方式)がそのまま使えます:

```bash
sftp demo@host -P 2222
scp app.tar.gz demo@host:/root/
scp demo@host:/root/app.tar.gz ./
scp -r ./site demo@host:/srv/
```

- 停止中のコンテナは転送前に**オンデマンドで起動**します。
- セッションはコンテナのホーム(`/root` 既定)で `root` として開始するため、
  相対パスは対話ログインと同様に解決します。共有ディレクトリや `/tmp`、
  シンボリックリンクも `nsenter` 経由でコンテナの namespaces 内で解決されます。
- 転送はコンテナ内の `sftp-server`(イメージの `openssh` パッケージ)に
  ブリッジされるため、専用クライアントは不要です。
- 制御ユーザー(`sandbox@host`)の `sftp` は未対応です。背後に対応する
  コンテナが無いためです。

## アーキテクチャ

```
             ssh sandbox@host:2222 ls
             ssh demo@host:2222
                     │
             ┌───────▼────────┐
             │  sandboxxing   │  Go / x/crypto/ssh
             │    daemon      │
             └───┬───────┬────┘
    管理コマンド │       │ nsenter(1)
                 ▼       ▼
        ┌────────────┐  ┌───────────────────────────┐
        │ pacstrap   │  │ systemd-nspawn コンテナ   │
        │ イメージ   │  │  (Arch Linux, ext4 image) │
        └────────────┘  └───────────────────────────┘
                 │              │
                 ▼              ▼
        /var/lib/sandboxxing/  bridge sbx0 + nft NAT
```

- **コンテナの実体**: `systemd-nspawn --image=<name>.img`。各コンテナは
  golden ext4 イメージの reflink コピー(`cp --reflink=auto`)を `resize2fs`
  で要求サイズまで成長させたものです。
- **起動**: `systemd-run` による transient unit (`sandboxxing-<name>`)。
  `MemoryMax` と `CPUQuota` でリソースを制限します。
- **ネットワーク**: ホストに bridge (`sbx0`) を作成し、コンテナ側は
  `--network-veth --network-bridge` で接続。nftables で masquerade します。
  コンテナの IP は sandboxxing が静止割り当てします。
- **コマンド実行**: `nsenter --target <leader> --mount --uts --ipc --net --pid
  --root --wdns=/`。PTY はコンテナ自身の `/dev/ptmx` から確保するため、
  コンテナ内の `/dev/pts/<n>` として見え、`tty` や `ttyname(3)` が
  正常に動作します。
- **状態**: `/var/lib/sandboxxing/state.json` にコンテナのメタデータを保存。

## 設定リファレンス (`/etc/sandboxxing/config.json`)

| キー | 既定値 | 説明 |
| --- | --- | --- |
| `data_dir` | `/var/lib/sandboxxing` | データのルート |
| `state_file` | `<data_dir>/state.json` | コンテナメタデータ |
| `ssh_addr` | `:2222` | SSH の待ち受けアドレス |
| `domain` | (空) | `ls` などに表示するホスト名 |
| `admin_users` | `["sandbox","admin"]` | 制御コマンドを許可するユーザー名 |
| `password` | (空) | 共有パスワード。省略/文字列で有効、空なら自動生成、**null で無効化** |
| `password_file` | `<data_dir>/password` | 自動生成パスワードの保存先 |
| `authorized_keys` | `<data_dir>/authorized_keys` | 公開鍵ファイル。省略で既定パス、**null で無効化**、指定して不在なら起動エラー |
| `bridge` | `sbx0` | コンテナ用ブリッジ |
| `subnet` | `10.100.0.0/16` | コンテナ用サブネット(/24 以上) |
| `dns` | (空) | コンテナに書き込むリゾルバ。空ならホストの resolv.conf を流用(ホスト専用の `127.0.0.53` は除外) |
| `shares` | (空) | 全コンテナに bind mount するホストディレクトリ。パス文字列または `{"path","target","read_only"}` |
| `tmp_size` | (空) | `/tmp` の扱い。空なら nspawn 既定の tmpfs(ホストメモリの10%)、サイズ/割合で変更、`"disk"` でコンテナイメージ上に保存 |
| `nested` | `false` | 全コンテナで Docker などのコンテナエンジンを**コンテナ内で**動かせるようにする(`new --nested` で個別指定)。nspawn の seccomp を `bpf` と keyring 向けに緩める |
| `image_dir` | `<data_dir>/images` | golden ext4 イメージのキャッシュ(`new` 時に reflink コピー) |
| `image` | `arch` | 既定イメージ名 |
| `mirror` | `https://geo.mirror.pkgbuild.com/$repo/os/$arch` | pacman ミラー |
| `arch` | 自動判定 | pacstrap のアーキテクチャ |
| `pacman_config` | (空) | 独自 pacman.conf(空なら生成) |
| `image_packages` | `base systemd openssh sudo vim iproute2 iputils dnsutils net-tools git curl ca-certificates` | イメージに追加するパッケージ |
| `pacstrap_timeout` | `30m` | イメージビルドの上限時間 |
| `boot_timeout` | `2m` | コンテナ起動待ちの上限 |
| `default_cpu` | `2` | `new` の既定 CPU |
| `default_memory` | `2G` | `new` の既定メモリ |
| `default_disk` | `10G` | `new` の既定ディスク |
| `log_level` | `info` | `debug` / `info` / `warn` / `error` |
| `default_backend` | `nspawn` | `new` の既定バックエンド(`nspawn` / `microvm`) |
| `microvm` | (なし) | MicroVM バックエンドの設定。[AWS Lambda MicroVM バックエンド](#aws-lambda-microvm-バックエンド)を参照 |

## ポートフォワーディング

同じプロトコルを話すため、OpenSSH のトンネルが両方向とも使えます。

```bash
# ローカルフォワード (ssh -L): コンテナ内のサービスを手元から参照
ssh -L 8080:localhost:80 demo@host -p 2222
# これで http://localhost:8080 がコンテナ demo の 80 番に届きます

# localhost 以外を指定するとホストから接続されます
ssh -L 15432:db.internal:5432 sandbox@host -p 2222

# リモートフォワード (ssh -R): 手元のサービスをホスト経由で公開
ssh -R 9000:localhost:3000 sandbox@host -p 2222

# ポートをデーモンに選ばせることもできます
ssh -R 0:localhost:3000 sandbox@host -p 2222   # "Allocated port 41234" と表示
```

- **コンテナ名で `ssh -L`** (`demo@host`): `localhost` はコンテナ自身を指す
  ため、`-L 8080:localhost:80` は `demo` 内の 80 番に届きます。停止中の
  コンテナは起動してから接続します。
- **制御ユーザーで `ssh -L`** (`sandbox@host`): ホストから接続するため、
  コンテナのアドレス、ホストのサービス、外部ネットワークに到達できます。
- **`ssh -R`**: デーモンが listen し、接続をクライアントへ転送します。
  バインドアドレスが空または `*` なら全インタフェース、`localhost` なら
  ホスト内のみ。SSH 接続の終了とともに全リスナーが閉じられます。

## ホストディレクトリの共有

ホストのディレクトリをコンテナに bind mount できます。設定で全コンテナに
適用する方法と、`--share` でコンテナ個別に指定する方法があります。

```json
{
  "shares": [
    "/srv/projects",
    { "path": "/srv/cache", "target": "/cache", "read_only": true }
  ]
}
```

```bash
# 特定のコンテナだけ
ssh sandbox@host -p 2222 new --name=demo --share=/srv/data
ssh sandbox@host -p 2222 new --name=demo --share=/srv/data:/data:ro
```

- 短縮形 `"/srv/projects"` は `/shared/projects` にマウントされます。
- `path[:target][:ro]` でコンテナ内パスと読み取り専用を指定できます。
- 同じ target のコンテナ個別設定はグローバル設定を上書きします。
- ホストのディレクトリを直接使うため、変更は即座に両側へ反映されます。
  コピーやコンテナ単位のオーバーレイは行いません。
- 既存コンテナへの追加は再起動(`restart <name>`)で反映されます。
  ホスト側のディレクトリは事前に作成してください(`-check` で確認できます)。

## コンテナの /tmp のサイズ

`systemd-nspawn` は各コンテナの `/tmp` を **メモリ上の tmpfs** として
マウントします。既定のサイズはホストメモリの 10% で、16GB のホストでは
1.6GB になります。ビルドや一時ファイルを多く書くエージェントでは不足
しがちです。

```json
{
  "tmp_size": "16G"
}
```

| `tmp_size` | 動作 |
| --- | --- |
| (空) | nspawn 既定の tmpfs(ホストメモリの 10%) |
| `8G`, `512M`, `1T` | 指定サイズの tmpfs |
| `50%` | ホストメモリに対する割合の tmpfs |
| `"disk"` | **tmpfs を使わない**。`/tmp` はコンテナのディスクイメージ上 |

`"disk"` は tmpfs を無効化する(`SYSTEMD_NSPAWN_TMPFS_TMP=0`)ため、`/tmp` は
コンテナの ext4 イメージ上の通常ディレクトリになります。容量はコンテナの
ディスク(`new` の `--disk`、変更は `resize --disk`)で決まり、**再起動しても
データが残ります**。一方 tmpfs の `/tmp` はコンテナ停止時に失われます。

絶対値とパーセント指定は nspawn が作成したマウントの上に重ねて適用される
ため、起動中のコンテナも次回起動時に反映されます。

## ネストしたコンテナ (サンドボックス内の Docker)

Docker や Podman などのコンテナエンジンを **sandbox コンテナの中で**動かせ
ます。ホスト全体で有効にする場合:

```json
{
  "nested": true
}
```

コンテナ単位で有効にする場合:

```bash
ssh sandbox@host -p 2222 new --name=dockerbox --nested
```

いずれの場合も、sandboxxing は次の設定でコンテナを起動します:

- **seccomp フィルタで `bpf` を許可**します。cgroup v2 のホストではデバイス
  コントローラが eBPF プログラムで実現されており、`runc` はコンテナごとに
  それを導入するため `bpf()` を呼びます。`systemd-nspawn` は既定でこの
  システムコールを塞ぐため、許可するまで `docker run` は必ず
  `bpf_prog_query(BPF_CGROUP_DEVICE): operation not permitted` で失敗します。
  ネストを可能にするのはこの設定です。
- **keyring 関連のシステムコール (`@keyring`) を許可**します(一部のイメージが
  利用します)。
- ホストに `/dev/fuse` があれば**パススルー**し、既定の `overlay2` が使えない
  場合のストレージドライバとして `fuse-overlayfs` を選べるようにします。

コンテナ側にコンテナエンジンが入っている必要があります。イメージは
`image_packages` から作られるので、ここに追加します(後から `pacman` で
入れても構いません):

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

- 動くのは rootful なエンジンです。rootless エンジンは sandbox 内では
  サポートしません。
- コンテナのルートは ext4 イメージなので、Docker 既定の `overlay2` ストレージ
  ドライバが使えます。`vfs` にフォールバックする場合は `fuse-overlayfs` を
  入れて選択してください。
- 既存のコンテナは次回起動まで nspawn 既定の seccomp フィルタのままです。
  ホスト全体で `nested` を有効にしてからデーモンを再起動
  (`systemctl restart sandboxxing`)し、既存コンテナには `restart <name>` で
  反映させてください。

## AWS Lambda MicroVM バックエンド

コンテナをこのホストの systemd-nspawn ではなく、
[AWS Lambda MicroVMs](https://docs.aws.amazon.com/lambda/latest/dg/lambda-microvms-guide.html)
(Firecracker の VM)として動かせます。バックエンドはコンテナごとに選びます:

```bash
ssh sandbox@host -p 2222 new --name=cloudbox --backend=microvm
ssh cloudbox@host -p 2222        # ログイン、scp/sftp、ssh -L はそのまま使えます
```

### しくみ

- **ホームディレクトリは EFS に置きます。** MicroVM は最長 8 時間で終了し、
  ディスクも失われます。そのためコンテナごとに EFS の access point
  (`/homes/<instance-id>`)を作り、MicroVM の起動時に `/root` へマウントします。
- **8 時間に達すると新しい MicroVM へ移行します。** Lambda が MicroVM を
  終了するとき、VM 内のエージェントが `/terminate` hook で受け取ります。
  エージェントはデーモンに通知し、データを flush して unmount します。
  デーモンは Lambda に状態を問い合わせて終了を確認したうえで、同じ
  access point をマウントする新しい MicroVM を起動します。デーモンが
  止まっていた場合は、次回起動時の照合か、次のログイン時に起動します。
- **SSH 接続のない MicroVM は自動停止します(autostop)。** すべての SSH 接続を
  中継するデーモンが、autostop 時間のあいだ接続がなかったコンテナの MicroVM を
  停止します。autostop 時間は既定で 30 分です。`new --autostop` か
  `edit --autostop` で変更でき、`0` にすると停止しません。接続があるとタイマーは
  リセットされます。管理セッションから `ssh <name>` で実行したコマンドも接続として
  数えます。停止後は、次のログインで新しい MicroVM を起動します。判定は 30 秒
  ごとに行い、デーモンを再起動するとタイマーは最初から数え直します。
- **VM 内のエージェント** (`cmd/sandboxxing-agent`) が MicroVM の HTTPS
  エンドポイント越しに WebSocket でコマンド実行、端末、ポート接続を提供します。
  エンドポイントは JWE トークン(デーモンだけが持ち、データポート 8080 のみ
  有効)と、MicroVM ごとの secret の両方で認証します。

### 制約

- **移行時には数十秒の断が生じます。** 実行中のプロセス、ルートファイル
  システムへの変更(後から入れたパッケージなど)、SSH 接続は引き継がれません。
  引き継がれるのは `/root` だけです。8 時間到達前に AWS から届く通知は
  ありません(届くのは終了時の hook だけです)。
- サイズは MicroVM イメージで決まります。`--cpu/--memory/--disk/--share/
  --nested`、`cp`、`resize` は使えません。
- ARM64 のみです。利用できるリージョンも限られます。
- `rm` はホームディレクトリの中身、access point、実行ロールを削除します。
  中身を消すために一時的に MicroVM を起動します。

### 分離

サンドボックスの利用者は VM 内で root なので、VM 内の認証情報もマウント操作も
自由に使えます。そこで他のコンテナのホームディレクトリに触れないよう、次の
対策をとっています。

- **コンテナごとに IAM 実行ロールを作ります。** このロールが許されるのは、
  自分の access point 経由のマウント(`elasticfilesystem:AccessPointArn`
  条件付き)と自分のログストリームへの書き込みだけです。デーモンは
  permissions boundary を付けたロールしか作れません。
- **EFS のファイルシステムポリシーには Deny だけを書きます**(TLS なしの
  アクセスと、access point を経由しないアクセスを拒否します)。ポリシーが
  存在するので匿名マウントも拒否されます。
- **access point のパスにはランダムな instance ID を使います。** 同じ名前で
  作り直しても、前のコンテナのファイルは見えません。
- VM 内から偽の終了通知を送っても、Lambda 側で終了を確認できない限り
  移行は起きません。

### セットアップ

AWS 側のリソースは `deploy/cdk` の CDK スタック(TypeScript)にまとまって
います。すべてのリソースに `RemovalPolicy.DESTROY` が付いているので、
`make cdk-destroy` でホームディレクトリのデータも含めて削除されます。

```bash
make cdk-install
aws lambda-microvms list-managed-microvm-image-versions \
  --image-identifier arn:aws:lambda:<region>:aws:microvm-image:al2023-1
make cdk-deploy BASE_IMAGE_VERSION=<version>
```

1. 出力 `DaemonPolicyArn` のポリシーを、デーモンが使う IAM ID にアタッチします。
2. 出力 `DaemonConfig` を `/etc/sandboxxing/config.json` にマージします。
3. `sandboxxing -check` で認証情報を確認してから、デーモンを再起動します。

デーモンは通常の AWS SDK の方法(環境変数、`~/.aws`、インスタンスロール)で
認証情報を取得します。

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

### 実機での確認結果

2026-10 に ap-northeast-1 で確認した結果です。

- **確認済み**: MicroVM 内の efs-utils は、コンテナごとの実行ロールを使って
  `tls,iam,accesspoint=` でマウントできます。別のサンドボックスの root から、
  次のマウントはいずれも `access denied` で拒否されました。
  - 他のコンテナの access point を指定したマウント
  - access point を使わない FS ルートのマウント
  - 匿名でのマウント
  - TLS なしの NFS マウント
- **確認済み**: 最大期間に達すると `/terminate` hook が届きます。その時点では
  エージェントへの WebSocket もまだ使えるので、デーモンに通知が届きます。
  通知は期限の約 20 秒後に届き、新しい MicroVM への移行は約 5 秒で終わりました。
- **確認済み**: イメージは `INTERNET_EGRESS` でビルドし、実行時には VPC
  connector を指定できます(インターネットへは NAT 経由)。
- **確認済み**: network connector の operator role は `lambda.amazonaws.com`
  を信頼すれば動きます。ただし connector の作成には 15 分ほどかかります。
- **確認済み**: `cdk destroy` で、スタックの全リソースが削除されます。
  デーモンが削除し残した access point と実行ロールも、あわせて削除されます。
- **判明した仕様**: MicroVM のエンドポイントは、無通信の WebSocket を 60 秒で
  切断します。デーモンは 20 秒ごとに ping を送ります。
- **判明した仕様**: connector を指定して RunMicrovm を呼ぶには、ドキュメントに
  載っていない `lambda:PassNetworkConnector` 権限が必要です。また、
  CreateAccessPoint にタグを付けると、`elasticfilesystem:TagResource` は
  ファイルシステムに対して評価されます。
- **判明した仕様**: 作成直後の実行ロールは、RunMicrovm には受け付けられても、
  VM の起動時に「role を assume できない」として失敗することがあります。
  初回の起動は再試行します。
- **未確認**: idle による suspend と WebSocket の関係(`idle_policy` は既定で
  無効)、CloudTrail の data event に `runHookPayload` が記録されるかどうか。

## コンテナ内の DNS

コンテナは `systemd-resolved` を動かしません。デーモンが `/etc/resolv.conf`
を直接書き込みます。`dns` が空の場合、ホストの `/etc/resolv.conf` を読み、
**ホスト自身でしか応答できないアドレスを除外**します。これは
`systemd-resolved` を動かしているホストで重要です。resolved のスタブ
(`127.0.0.53`) はコンテナのネットワーク名前空間からは到達できないため、
それを指定すると IP の疎通があっても名前解決だけが失敗します。

自動選択が望ましくない場合は明示的に指定します:

```json
{
  "dns": ["1.1.1.1", "8.8.8.8"]
}
```

既存のコンテナも次回起動時に新しい設定を反映します。

## セキュリティ上の注意

- デーモンは **root で動作**し、`ssh_addr` は既定で全インタフェースに
  バインドします。インターネットに直接公開せず、SSH ポートの転送や
  VPN、ファイアウォールの背後で使ってください。
- コンテナは user namespace を使わずに起動します(`-U` を付けません)。
  ホスト側の分離は nspawn の namespaces と cgroups に依存します。
  信頼できないワークロードを動かす場合は、追加の分離設定を検討して
  ください。
- `nested` は nspawn の seccomp フィルタを緩めます。`bpf` と keyring の
  システムコールは名前空間で隔離されないため、ホストカーネル上でも許可
  されます。自前のエンジンを動かす必要があるコンテナにのみ有効化して
  ください。
- コンテナ名はユーザー名として使われるため、`admin_users` と同じ名前の
  コンテナは作成できません。
- **公開鍵認証を推奨します**。パスワードは `admin_users` とコンテナ名の
  両方で共通の単一シークレットなので、漏洩すると全コンテナに影響します。
  `password: null` + `authorized_keys` の指定でパスワード認証を止められます。
- `authorized_keys` は sshd(8) と同じ形式です。`no-pty` や
  `command=` などのオプションは解釈されず、鍵の部分だけが使われます。

## ライセンス

MIT
