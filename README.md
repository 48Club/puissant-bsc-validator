## 48Club puissant-bsc-validator

The repo is based on bnb-chain/bsc:master, maintained by 48Club.

The repo is modified and aiming to implement the puissant service. It is for validator use only.

We guarantee that the repo follows every bnb-chain/bsc new release ASAP.

You can always [compare](https://github.com/bnb-chain/bsc/compare/master...48Club:puissant-bsc-validator:develop) the code with bnb-chain/bsc:master to check the difference.

Contributions are welcome.

### Mainly changes for 48Club puissant service

- **Added Configs**:
-
   * `TxPool.TrustRelays`: list of relay addresses for accepting puissant package from
   * `TxPool.MaxPuissantPreBlock`: max puissant packages per miner commit job，default is 25
   * `Miner.NodeAlias`: node name that shows on telegram channel/group
   * `Miner.TelegramKey`: telegram bot api-key
   * `Miner.TelegramToID`: telegram group/channel id
