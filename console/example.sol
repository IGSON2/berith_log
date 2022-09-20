pragma solidity ^0.5.2;

contract AdditionGame {

    constructor() public payable {

    }
    
    function getBalance() public view returns (uint) {
        return address(this).balance;
    }
 
  
    function transfer(uint _value) public payable returns (bool) {
        require(getBalance() >= _value);
        address payable receiver = msg.sender;
        receiver.transfer(_value);
        return true;
    }
}