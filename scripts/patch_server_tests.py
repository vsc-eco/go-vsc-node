path = "/home/ubuntu/vsc/go-vsc-node/scripts/testnet-multinode-setup.py"
with open(path) as f:
    s = f.read()
old = '_c.known_chains["HIVE"] = tst_chain\n            _orig_get_network'
insert = '''
            keys = kwargs.get("keys", [])
            kwargs["keys"] = []
            try:
                from beemstorage import InRamPlainKeyStore
                kwargs["key_store"] = InRamPlainKeyStore()
            except ImportError:
                pass
            '''
new = '_c.known_chains["HIVE"] = tst_chain' + insert + '\n            _orig_get_network'
if old not in s:
    print("NO_MATCH")
else:
    s = s.replace(old, new, 1)
    old2 = "h = Hive(**kwargs)\n                h.get_network"
    new2 = "h = Hive(**kwargs)\n                if keys:\n                    h.wallet.setKeys(keys)\n                h.get_network"
    s = s.replace(old2, new2, 1)
    with open(path, "w") as f:
        f.write(s)
    print("PATCHED")
