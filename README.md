## 48Club puissant-bsc-validator

The repo is based on bnb-chain/bsc:master, maintained by 48Club.

The repo is modified and aiming to implement the puissant service. It is for validator use only.

We guarantee that the repo follows every bnb-chain/bsc new release ASAP.

You can always [compare](https://github.com/bnb-chain/bsc/compare/master...48Club:puissant-bsc-validator:develop) the code with bnb-chain/bsc:master to check the difference.

Contributions are welcome.

### Mainly deviation for original bnb-chain bsc repo

- **Additional Configs**:
   * `TxPool.TrustRelays`: list of trusted relayer addresses. Default value is 48Club's relayer address.
   * `TxPool.MaxPuissantPreBlock`: max puissant packages capacity per miner commit job，default is 25.
   * `Miner.TelegramKey`: Optional. You can set up a telegram bot to report puissant events. Fill bot token here.
   * `Miner.TelegramToID`: Optional. ID of the telegram user/channel/group you want to send report to.
   * `Miner.NodeAlias`: Optional. Node name that shows in Telegram report.
### Already been a validator? Check all the 4 steps needed to join puissant and raise your APR now.
  ## Establish private network with a relayer.
  Different relayer may require different material. For 48Club relayer: 
  * Send an application via this [portal](https://www.google.com) //TODO, providing 
    1. Contact of your team.
    2. Validator consensus address.
    3. Wireguard public key for future private network.
    4. RPC Port you would config.
  * 48Club will response with a wireguard configuration file leaving the private key to be filled, which should be paired with the previous public key in the application.
  * Start wireguard on the same host where your validator instance runs. Notice that only one instance at the same time, otherwise the system would not work as expected.
  * Feel free to contact 48Club. 
  ## Replace original bsc-geth with binary compiled from this very repo.
  Clone and compile and replace.
  ## Expose RPC port and config it properly.
  Make sure rpc port is configed aligned with the previously provided value, in `config.toml` or command line parameters whichever is your case.
  Make sure it is properly exposed to the private network built by wireguard before. If you did enable it, just check the firewall; Otherwise edit `config.toml` or simply add a `--http.addr <port>` parameter.
  ## Ask relayer to start feeding puissant data.
  Contact 48Club to do the final test and start feeding.
