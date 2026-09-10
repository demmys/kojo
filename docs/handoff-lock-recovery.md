# 短時間往復と強制復帰の所有権回復

## 原因

出発元の `complete` は、その端末のDBに移動先をholderとする5分leaseを残す。
この行はSlack添付の中継認可にも使うため、出発直後に失効させてはいけない。
一方、5分以内に同じ端末へ戻ると、通常の `AcquireAgentLock` ではこの行を取得できず、finalizeが失敗していた。
後からGuardだけが取得するとproxyがselfになり、未完了のfinalizeを経由せずにSlack代理認可が壊れた。

別の不具合として、強制復帰でDBをlocalへ戻しても、Slackルーターのメモリhintが古い移動先を指し続けた。
403は再送してよい証拠ではないため、全403を再送する修正は採らない。

## 受信側の永続receipt

`incoming_handoffs` は端末ローカルの移動受入記録で、peer間では同期しない。
同じsnapshot transactionで、agent ID、op ID、認証source、target、受信側の既存holderと永続fencing counterを束縛する。
別端末のcounter同士は比較しない。

```text
phase-1 snapshot → prepared
source complete/drain → finalize → accepted → activated → done
                                holder/proxy/tokenを原子的に確定
prepared → drop → aborted
```

- `prepared`：通常のGuard取得とターン受付を禁止する。
- `accepted`：holderとHub proxyは確定したが、token採用やruntime登録は未完了。起動時の自動復帰とターン受付を禁止する。
- `activated`：runtime hook完了。到着ターンを受付可能にして、既存の到着配信プロトコルへ進む。
- `done`：到着処理まで完了。同じopを再度activateしない。
- `aborted`：同世代のsnapshotを自動起動しない。後続の正当な受入または明示的な強制復帰による世代更新で、この起動制限は効かなくなる。

finalize再試行はreceiptのsourceと世代を検証してからruntimeを読み直す。
強制復帰や別の所有権取得でcounterが進んだら、古いfinalizeは拒否する。
正常な停止でlock行だけ消えた場合は、counterが変わっていないことを確認して再取得する。

`drop` はpending credentialsのKV保存に依存しない。
phase-1より先に届いたキャンセルも、agent行を必要としないsource付きtombstoneとして保存する。
未知のop同士には順序を付けられないため、同世代の未完了opを新しいsnapshotで自動上書きしない。
キャンセル応答が失われても同じdropを再試行できるが、drop自体が届かなければ次の移動前に明示的なキャンセルが必要になる。

## 複数端末を経由する帰還

A→B→C→Aでは、Aに残ったholderがBのままでも正常である。
AはBのDBに基づくreadiness routeをデバイス認証付き通信で照会し、Cへの委譲を確認する。
最大8 hopまで追跡し、循環、照会失敗、sourceとの不一致は拒否する。
確認結果はAの既存世代に束縛し、snapshot transactionとfinalizeで再検証する。
state probeはlockやruntimeをpurgeせず、全量snapshotを要求する。
A自身がholderなら、古いsourceの照会でその所有権を消すことはない。

## 遅延処理の制限

Slack hintには、その端末のholderと永続counterを付ける。
探索やPOSTより前に取得した世代だけを応答処理に使い、古い応答が新世代の経路を学習させないようにする。
世代不一致のhintは次のroutingで使わない。
すでに送信したPOSTは、到達した可能性があるため別holderへ無条件に再送しない。

出発元の遅延release、abort、未送信のfinalizeにも、取得時またはcomplete時の世代を使う。
GuardのAdd、Remove、refreshもagent単位で直列化し、Remove前のsnapshotによる遅延取得を止める。

## 適用条件と限界

- 修正対象のHubと参加peerをすべて更新してから往復を確認する。旧targetの通常取得処理はこの修正では変わらない。
- 旧版で保存された世代情報のないpending finalizeは409で拒否する。既に壊れた移動を無条件再実行せず、正しい所有者を確認して強制復帰し、新しい移動を開始する。
- これはv1の端末ローカルfencingと委譲確認であり、分散コンセンサスではない。すでに遠隔端末へ送信されたPOSTを、別端末の強制復帰が遡って取り消す保証はない。
- 受入と到着配信は別段階である。到着応答が不確定な場合は、既存の重複防止に従い自動再送を抑制する。

## 回帰テスト

`incoming_handoffs_test.go`、`agentlockguard_test.go`、`handoff_recovery_test.go`で次を検証する。

- 2 Store間の5分未満の往復と原子的なproxy設定
- accepted/activatedからの停止後再試行
- 強制復帰後の古いfinalize、release、abort、Slack応答の拒否
- phase-1より先のdrop、credentials保存前のdrop、未知の旧opによる上書き拒否
- 3 peer帰還の委譲確認と、照会中に起きたlocal強制復帰の保護
- owner権限があるデバイスでもsource IDを偽装できないこと
