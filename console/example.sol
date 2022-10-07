pragma solidity ^0.8.13;

contract AdditionGame {

    constructor() public payable {

    }
    
    function getBalance() public view returns (uint) {
        return address(this).balance;
    }
 
  
    function transfer(uint _value) public payable returns (bool) {
        require(getBalance() >= _value);
        payable(msg.sender).transfer(_value);
        return true;
    }
}