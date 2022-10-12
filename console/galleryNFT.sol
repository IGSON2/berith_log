// SPDX-License-Identifier: MIT

pragma solidity ^0.8.1;

import "hardhat/console.sol";
import "@openzeppelin/contracts/token/ERC721/ERC721.sol";
import "@openzeppelin/contracts/token/ERC721/extensions/ERC721Enumerable.sol";
import "@openzeppelin/contracts/utils/Counters.sol";

contract GalleryNft is ERC721Enumerable{
    using Strings for uint256;
    using Counters for Counters.Counter;
    Counters.Counter private _tokenIds;

    SaleInfo public saleInfo;

    address public owner;
    address public admin;

    bool public submitMinting = false;
    bool public lockNft = false;
    bool public revealed = false;

    string baseUri = "";
    string unRevealUri = "";

    struct SaleInfo {
        uint256 price;
        uint256 maxSupply;
        uint256 maxMintAmount;
        uint256 maxMintPerSale;
    }

    mapping(address => bool) whiteList;

    constructor(
        string memory _name,
        string memory _symbol,
        string memory _baseUri,
        string memory _unRevealUri,
        bool _lockNft,
        bool _submitMinting,
        bool _revealed,
        SaleInfo memory _saleInfo,
        address _admin
    ) ERC721(_name, _symbol) {
        baseUri = _baseUri;
        unRevealUri = _unRevealUri;
        submitMinting = _submitMinting;
        lockNft = _lockNft;
        revealed = _revealed;
        owner = msg.sender;
        saleInfo = _saleInfo;
        admin = _admin;
    }

    // modifier
    modifier limitAdmin() {
        require(admin == msg.sender || owner == msg.sender, "Only admin & owner can call this.");
        _;
    }
    modifier limitOwner() {
        require(owner == msg.sender, "Only owner can call this");
        _;
    }

    // setter
    function setLockNft(bool _isLock)
    public limitOwner {
        lockNft = _isLock;
    }
    function setSubmitMinting(bool _isMinting)
    public limitOwner {
        submitMinting = _isMinting;
    }

    // getter
    function isUserWhiteList(address _user)
    public view
    returns (bool){
        return whiteList[_user];
    }

    // public
    function transferFrom(
        address from,
        address to,
        uint256 tokenId
    ) public virtual override{
        require(lockNft == false);
        require(_isApprovedOrOwner(_msgSender(), tokenId), "ERC721: transfer caller is not owner nor approved");
        _transfer(from, to, tokenId);
    }

    function publicMint(uint256 _requestedAmount) public payable {
        uint256 nowTokenId = _tokenIds.current();
        require(submitMinting, "The public sale is not enabled!");
        require(nowTokenId + _requestedAmount <= saleInfo.maxSupply + 1, "Exceed max amount");
        require(_requestedAmount > 0 && _requestedAmount <= saleInfo.maxMintAmount, "Too many requests or zero request");
        require(msg.value == saleInfo.price * _requestedAmount, "Not enough eth");
        require(balanceOf(msg.sender) + _requestedAmount <= saleInfo.maxMintPerSale, "Exceed max amount per person");

        for (uint256 i = 1; i <= _requestedAmount; i++) {
            _safeMint(msg.sender, nowTokenId + i);
            _tokenIds.increment();
        }
    }

    function whiteListSale(uint256 _requestedAmount) public payable {
        uint256 nowTokenId = _tokenIds.current();
        require(isUserWhiteList(msg.sender), "this address is not whitelist user");
        require(submitMinting, "The whitelist sale is not enabled!");
        require(nowTokenId + _requestedAmount <= saleInfo.maxSupply + 1, "Exceed max amount");
        require(_requestedAmount > 0 && _requestedAmount <= saleInfo.maxMintAmount, "Too many requests or zero request");
        require(msg.value == saleInfo.price * _requestedAmount, "Not enough eth");
        require(balanceOf(msg.sender) + _requestedAmount <= saleInfo.maxMintPerSale, "Exceed max amount per person");

        for (uint256 i = 1; i <= _requestedAmount; i++) {
            _safeMint(msg.sender, nowTokenId + i);
            _tokenIds.increment();
        }
    }

    function airDropMint(address _receiver, uint256 _requestedCount)
    public limitAdmin {
        require(_requestedCount > 0, "zero request");
        uint256 tokenId = _tokenIds.current();
        require(tokenId + _requestedCount <= saleInfo.maxSupply + 1, "Exceed max amount");

        for (uint256 i = 1; i <= _requestedCount; i++) {
            _mint(_receiver, tokenId + i);
            _tokenIds.increment();
        }
    }

    function tokenURI(uint256 _tokenId) public view virtual override
    returns (string memory) {
        if (!revealed) {
            return bytes(unRevealUri).length > 0
            ? string(abi.encodePacked(unRevealUri, _tokenId.toString(), ".json"))
            : "";
        }

        return bytes(baseUri).length > 0
        ? string(abi.encodePacked(baseUri, _tokenId.toString(), ".json"))
        : "";
    }

    function addUserWhiteList(address _receiver) public limitAdmin {
        whiteList[_receiver] = true;
    }

    function removeUserWhitelist(address _receiver) public limitAdmin {
        delete whiteList[_receiver];
    }

    function setSaleInfo(SaleInfo memory _saleInfo) public limitAdmin {
        saleInfo = _saleInfo;
    }

    function getSaleInfo() public view returns (SaleInfo memory){
        return (saleInfo);
    }

    function walletOfOwner(address _owner) external view returns (uint256[] memory) {
        uint256 tokenCount = balanceOf(_owner);
        uint256[] memory tokensId = new uint256[](tokenCount);
        for (uint256 i = 0; i < tokenCount; i++) {
            tokensId[i] = tokenOfOwnerByIndex(_owner, i);
        }
        return tokensId;
    }
}