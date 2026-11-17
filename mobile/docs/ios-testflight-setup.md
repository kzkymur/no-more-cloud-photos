# iPhone に入れるための準備（お客様向け手順書）

アプリ「NMCP Photos」を TestFlight からお客様の iPhone に入れるため、一度だけ次の設定をお願いします。所要時間はおよそ 30〜60 分です（Apple の審査待ち時間は含みません）。

設定が終わると、`main` ブランチが更新されるたびに GitHub Actions がアプリを自動でビルドし、TestFlight に届けます。
お客様の Mac や Xcode は不要です。

## 1. Apple Developer Program に登録する（未登録の場合）
1. https://developer.apple.com/programs/ から登録します（年額 99 米ドル。有効になるまで最大 48 時間ほどかかることがあります）。
2. 登録が完了したら https://developer.apple.com/account の「メンバーシップの詳細」を開き、**チーム ID**（英数字 10 文字）を控えてください。

## 2. Bundle ID（アプリの識別子）を決めて登録する
1. Bundle ID は Apple 全体で重複できません。お客様固有の名前をおすすめします。
   - 例: `com.<お名前やドメインを逆にしたもの>.nmcpphotos`（例 `com.yamada.nmcpphotos`）
   - 既定値の `io.nmcp.photos` は、すでに誰かが登録していると使えません。
2. https://developer.apple.com/account/resources/identifiers で「＋」を押し、次のように登録します。
   - 種類: 「App IDs」→「App」
   - Description: `NMCP Photos`
   - Bundle ID: 「Explicit」を選び、決めた値を入力
   - Capabilities: 何も追加しなくて大丈夫です
3. 決めた Bundle ID を控えてください。

## 3. App Store Connect にアプリを作る
1. https://appstoreconnect.apple.com/apps で「＋」→「新規 App」を選びます。
2. 次のように入力します。
   - プラットフォーム: iOS
   - 名前: `NMCP Photos`（App Store で他と重複しない名前が必要です。重複する場合は `NMCP Photos (<お名前>)` などにしてください）
   - 言語: 日本語
   - バンドル ID: 手順 2 で登録したもの
   - SKU: 任意（例 `nmcp-photos`）
   - アクセス: フルアクセス
3. 公開（審査提出）はしません。TestFlight の内部テストだけで使います。

## 4. App Store Connect API キーを発行する
1. https://appstoreconnect.apple.com/access/integrations/api で「キーを生成」を押します。
   - 名前: `GitHub Actions NMCP`
   - アクセス: **Admin**（証明書とプロファイルを自動で作るために必要です）
2. 次の 3 つを控えます。
   - **キー ID**
   - **Issuer ID**（一覧の上に表示されます）
   - ダウンロードした **`AuthKey_<キーID>.p8`** ファイル（**ダウンロードは 1 回しかできません**。パスワードマネージャー等に保管してください）

## 5. GitHub に設定を入れる（リポジトリ owner の作業）
https://github.com/kzkymur/no-more-cloud-photos/settings/environments を開きます。

### 5-1. environment `ios-release`
1. 「New environment」→ 名前 `ios-release` で作成します。
2. **Deployment branches and tags**: 「Selected branches and tags」→ `main` を追加します。
3. **Required reviewers**: お客様ご自身を追加します（ビルドのたびに承認ボタンを押す形になります。不要なら外しても動きます）。ここで設定すると、手順 6-2 で承認が必要になります。
4. **Environment secrets** に次の 4 つを追加します。

   | 名前 | 値 |
   |---|---|
   | `ASC_KEY_P8_BASE64` | `.p8` ファイルを base64 にした文字列（Mac のターミナルで `base64 -i AuthKey_XXXX.p8 \| pbcopy` を実行するとコピーされます） |
   | `ASC_KEY_ID` | キー ID |
   | `ASC_ISSUER_ID` | Issuer ID |
   | `APPLE_TEAM_ID` | チーム ID |

5. **Environment variables** に次の 1 つを追加します。

   | 名前 | 値 |
   |---|---|
   | `NMCP_IOS_BUNDLE_ID` | 手順 2 の Bundle ID |

### 5-2. environment `android-release`（Android 版の署名鍵を保護するため）
1. 「New environment」→ 名前 `android-release` で作成します。
2. Deployment branches を `main` のみにし、Required reviewers にお客様を追加します（5-1 と同じ）。
3. secrets の移し替えは開発側で行います。作成できたらご連絡ください。
   - environment を作るには admin 権限が必要ですが、secrets の値を入れるのは開発側でもできます。
   - 移し替えが終わったら、リポジトリ単位の同じ名前の secrets（`NMCP_ANDROID_*` の 3 つ）を削除します。

## 6. 最初のビルドを流す
1. https://github.com/kzkymur/no-more-cloud-photos/actions/workflows/mobile-ios.yml を開き、「Run workflow」→ ブランチ `main` で実行します。
2. 承認を求められたら「Review deployments」→ `ios-release` →「Approve」を押します。
3. 20〜40 分ほどで完了します。その後、Apple 側の処理に 5〜30 分かかり、App Store Connect の「TestFlight」タブにビルドが表示されます。
4. 輸出コンプライアンス: アプリに `ITSAppUsesNonExemptEncryption = NO`（標準の HTTPS 以外の暗号化は使っていない）を設定済みなので、通常は質問されません。万一ビルドに「輸出コンプライアンス情報がありません」と出た場合は、「暗号化を使用していない（標準の HTTPS のみ）」と回答してください。

## 7. サーバ側の準備（HTTPS）
iPhone 版は、セキュリティのため **https の URL** でサーバにつなぎます。Tailscale の HTTPS 証明書を使えるようにしてください。
1. Tailscale の管理画面 https://login.tailscale.com/admin/dns で **MagicDNS** と **HTTPS Certificates** を有効にします。
2. 自宅サーバの `./deploy`（`docs/ubuntu-deployment.md`）は `tailscale cert` で証明書を取得し、Core API を HTTPS で公開します。アプリに入れるサーバ URL は、deploy で設定した Tailnet の DNS 名とポートを使った `https://<ホスト名>.<tailnet>.ts.net[:ポート]` です。
3. iPhone を Tailscale に接続した状態で、Safari から `<サーバ URL>/readyz` を開きます。`{"status":"ok"}` のような応答が出れば準備完了です。http の URL や証明書のエラーが出る URL は、iPhone 版では使えません。

## 8. iPhone に入れる
1. App Store Connect の「TestFlight」→「内部テスト」でグループを作り、ご自身の Apple ID を追加します。
2. iPhone に App Store から **TestFlight** アプリを入れ、同じ Apple ID でサインインします。
3. TestFlight に「NMCP Photos」が表示されるので「インストール」を押します。
4. アプリを開き、次の 3 点を設定します。
   - サーバ URL（例 `https://<ホスト名>.<tailnet>.ts.net`）を入力
   - 写真へのアクセスで「**フルアクセスを許可**」を選ぶ
   - iPhone で Tailscale アプリを接続しておく
- TestFlight のビルドは 90 日で期限が切れます。`main` が更新されれば新しいビルドが自動で届きます。

## 困ったとき
- **GitHub Actions が「TestFlight upload skipped」という警告で終わる**: 手順 5-1 の secrets / variables のどれかが足りていません。警告文に、足りない名前が表示されます。
- **署名のエラー（`No profiles for ...` など）が出る**: 次の 3 点を確認してください。
  - API キーのアクセス権が Admin になっているか
  - Bundle ID が手順 2 で登録したものと完全に一致しているか
  - 手順 3 のアプリが作成済みか
- **それでも解決しないとき**: エラーの画面か、Actions のログの URL をそのまま開発側に送ってください。
