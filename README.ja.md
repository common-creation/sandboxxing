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
    [--comment=TEXT] [--tag=T] [--env K=V] [--setup-script=FILE]
    [--prompt=TEXT] [--json]
```

- 名前を省略すると `sbx-xxxxxx` が自動生成されます。
- 作成後すぐに起動し、IP アドレスが割り当てられます。
- `--setup-script` は初回にコンテナ内で実行するスクリプトです。
  `/dev/stdin` を指定すると標準入力から読み込みます。
- `--prompt` は作成直後にコンテナへ流し込む初期コマンドです。

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
cp <source> [new-name] [--cpu=N] [--memory=4G] [--disk=20G] [--copy-tags] [--json]
```

- 実行中のコンテナを複製することもできます(ディスクイメージをコピー)。
- `--copy-tags` は既定で有効です。無効にする場合は `--copy-tags=false`。

### `resize` — リソース変更

```
resize <name> [--cpu=N] [--memory=4G] [--disk=20G]
```

- CPU とメモリは systemd の transient unit プロパティ (`CPUQuota`,
  `MemoryMax`) として即時反映されます。
- ディスクは縮小できません。拡張はコンテナを停止し、`e2fsck` と
  `resize2fs` でイメージ内のファイルシステムを成長させます。

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
- コンテナ内の `sshd` は使いません。ホスト側の `nsenter` で
  namespaces に入るため、コンテナは起動していれば十分です。

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

- **コンテナの実体**: `systemd-nspawn --image=<name>.img`。ディスク
  イメージは `pacstrap` で作った rootfs から `mke2fs -d` で作成します。
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
| `image_dir` | `<data_dir>/images` | pacstrap ツリーのキャッシュ(`new` 時に ext4 イメージ化) |
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
- コンテナ名はユーザー名として使われるため、`admin_users` と同じ名前の
  コンテナは作成できません。
- **公開鍵認証を推奨します**。パスワードは `admin_users` とコンテナ名の
  両方で共通の単一シークレットなので、漏洩すると全コンテナに影響します。
  `password: null` + `authorized_keys` の指定でパスワード認証を止められます。
- `authorized_keys` は sshd(8) と同じ形式です。`no-pty` や
  `command=` などのオプションは解釈されず、鍵の部分だけが使われます。

## ライセンス

MIT
