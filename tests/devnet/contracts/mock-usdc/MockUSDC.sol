// SPDX-License-Identifier: MIT
pragma solidity 0.8.33;

// MockUSDC: the devnet stand-in for Circle's USDC (FiatTokenV2_2,
// github.com/circlefin/stablecoin-evm) with the behaviour the EVM bridge
// depends on: 6 decimals, Transfer events, and transfer / transferFrom that
// revert while paused or when the sender or the recipient is blacklisted, as
// FiatTokenV2_2's whenNotPaused / notBlacklisted(msg.sender) /
// notBlacklisted(to) do. Like FiatTokenV2_2's initializer, it blacklists its
// own address. No proxy, minter roles or permit: the deployer mints.
//
// balanceOf is storage slot 0, so a test can set a holder's balance with
// anvil_setStorageAt at keccak256(holder . 0).
contract MockUSDC {
    mapping(address => uint256) public balanceOf;
    mapping(address => mapping(address => uint256)) public allowance;
    mapping(address => bool) public isBlacklisted;
    uint256 public totalSupply;
    address public owner;
    bool public paused;

    string public constant name = "USD Coin";
    string public constant symbol = "USDC";
    uint8 public constant decimals = 6;

    event Transfer(address indexed from, address indexed to, uint256 value);
    event Approval(address indexed holder, address indexed spender, uint256 value);

    constructor() {
        owner = msg.sender;
        isBlacklisted[address(this)] = true;
    }

    modifier onlyOwner() {
        require(msg.sender == owner, "MockUSDC: not owner");
        _;
    }

    modifier whenNotPaused() {
        require(!paused, "Pausable: paused");
        _;
    }

    modifier notBlacklisted(address account) {
        require(!isBlacklisted[account], "Blacklistable: account is blacklisted");
        _;
    }

    function mint(address to, uint256 value) external onlyOwner {
        totalSupply += value;
        balanceOf[to] += value;
        emit Transfer(address(0), to, value);
    }

    function blacklist(address account) external onlyOwner {
        isBlacklisted[account] = true;
    }

    function unBlacklist(address account) external onlyOwner {
        isBlacklisted[account] = false;
    }

    function pause() external onlyOwner {
        paused = true;
    }

    function unpause() external onlyOwner {
        paused = false;
    }

    function transfer(address to, uint256 value)
        external
        whenNotPaused
        notBlacklisted(msg.sender)
        notBlacklisted(to)
        returns (bool)
    {
        _transfer(msg.sender, to, value);
        return true;
    }

    function approve(address spender, uint256 value)
        external
        whenNotPaused
        notBlacklisted(msg.sender)
        notBlacklisted(spender)
        returns (bool)
    {
        allowance[msg.sender][spender] = value;
        emit Approval(msg.sender, spender, value);
        return true;
    }

    function transferFrom(address from, address to, uint256 value)
        external
        whenNotPaused
        notBlacklisted(msg.sender)
        notBlacklisted(from)
        notBlacklisted(to)
        returns (bool)
    {
        require(value <= allowance[from][msg.sender], "ERC20: transfer amount exceeds allowance");
        allowance[from][msg.sender] -= value;
        _transfer(from, to, value);
        return true;
    }

    function _transfer(address from, address to, uint256 value) internal {
        require(from != address(0), "ERC20: transfer from the zero address");
        require(to != address(0), "ERC20: transfer to the zero address");
        require(value <= balanceOf[from], "ERC20: transfer amount exceeds balance");
        balanceOf[from] -= value;
        balanceOf[to] += value;
        emit Transfer(from, to, value);
    }
}
