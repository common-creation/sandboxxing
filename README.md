# sandboxxing

`exe.dev` のような開発用サンドボックスを、自分のホスト上で **systemd-nspawn**
を使って提供するデーモンです。クライアントアプリは不要で、操作はすべて
**SSH プロトコル**の上に実装されています。

```
ssh sandbox@host -p 2222 ls
ssh sandbox@host -p 2222 new --name=demo --cpu=4 --memory=8G
ssh demo@host -p 2222
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

4. アクセス用パスワードを確認する:

   ```bash
   sudo sandboxxing -show-password
   # あるいは: sudo cat /var/lib/sandboxxing/password
   ```

   パスワードは初回起動時に自動生成され、`password_file`
  に保存されます。`config.json` の `password` を指定すればそちらが優先されます。

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
| `password` | (空) | 共有パスワード(空なら自動生成) |
| `password_file` | `<data_dir>/password` | 自動生成パスワードの保存先 |
| `authorized_keys_file` | `<data_dir>/authorized_keys` | 接続を許可する公開鍵(存在する場合のみ公開鍵認証を有効化) |
| `bridge` | `sbx0` | コンテナ用ブリッジ |
| `subnet` | `10.100.0.0/16` | コンテナ用サブネット(/24 以上) |
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

## ライセンス

MIT
