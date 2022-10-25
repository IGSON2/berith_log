pragma solidity ^0.8.4;

contract AdditionGame {

    constructor() public payable {

    }

    receive() external payable {

    }
    
    function getBalance() public view returns (uint) {
        return address(this).balance;
    }
 
  
    function transferBrt(uint _value) public payable returns (bool) {
        require(getBalance() >= _value);
        payable(msg.sender).transfer(_value);
        return true;
    }
}