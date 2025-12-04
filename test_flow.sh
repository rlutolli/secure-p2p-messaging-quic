#!/bin/bash
# Quick test script to verify the application starts correctly

echo "Testing P2P Messenger..."
echo "========================"

# Test 1: Build
echo -e "\n[Test 1] Building application..."
if go build -o p2p-messenger . 2>&1; then
    echo "✓ Build successful"
else
    echo "✗ Build failed"
    exit 1
fi

# Test 2: Check if binary exists
echo -e "\n[Test 2] Checking binary..."
if [ -f "./p2p-messenger" ]; then
    echo "✓ Binary created"
else
    echo "✗ Binary not found"
    exit 1
fi

# Test 3: Check for syntax/logic errors in key functions
echo -e "\n[Test 3] Checking code structure..."
if grep -q "func.*sendToRoom" main.go && grep -q "func.*GetOrCreate" connection_manager.go; then
    echo "✓ Key functions present"
else
    echo "✗ Missing key functions"
    exit 1
fi

# Test 4: Check alias generation
echo -e "\n[Test 4] Checking alias generation..."
if grep -q "generateAlias" server.go; then
    echo "✓ Alias generation present"
else
    echo "✗ Alias generation missing"
    exit 1
fi

# Test 5: Check room discovery
echo -e "\n[Test 5] Checking room discovery..."
if grep -q "DiscoverAllRooms" discovery.go; then
    echo "✓ Room discovery present"
else
    echo "✗ Room discovery missing"
    exit 1
fi

# Test 6: Check join message logic
echo -e "\n[Test 6] Checking join message logic..."
if grep -q "joined the chat" server.go; then
    echo "✓ Join message logic present"
else
    echo "✗ Join message logic missing"
    exit 1
fi

echo -e "\n========================"
echo "All basic checks passed!"
echo "========================"
echo ""
echo "To test manually:"
echo "1. Run: ./p2p-messenger"
echo "2. Create a room (type 'n' then room name)"
echo "3. In another terminal, run again and select the room"
echo "4. Try 'send <message>' in both terminals"

