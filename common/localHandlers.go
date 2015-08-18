/**
 * Memproxy is a proxy for memcached that will split the data input
 * into fixed-size chunks for storage. It will reassemble the data
 * on retrieval with set.
 */
package common

import "bufio"
import "bytes"
import "encoding/binary"
import "fmt"
import "math"

func handleSet(cmd SetCmdLine, remoteReader *bufio.Reader, localReader *bufio.Reader, localWriter *bufio.Writer) error {
    // Read in the data from the remote connection
    buf := make([]byte, cmd.length)
    err := readDataIntoBuf(remoteReader, buf)
    
    numChunks := int(math.Ceil(float64(cmd.length) / float64(CHUNK_SIZE)))
    token := <-tokens
    
    if verbose { fmt.Printf("% x", token) }
    
    metaKey := makeMetaKey(cmd.key)
    metaData := Metadata {
        Length:    int32(cmd.length),
        OrigFlags: int32(cmd.flags),
        NumChunks: int32(numChunks),
        ChunkSize: CHUNK_SIZE,
        Token:     *token,
    }
    
    if verbose {
        fmt.Println("metaKey:", metaKey)
        fmt.Println("numChunks:", numChunks)
    }
    
    metaDataBuf := new(bytes.Buffer)
    binary.Write(metaDataBuf, binary.LittleEndian, metaData)
    
    if verbose {
        fmt.Printf("% x\r\n", metaDataBuf.Bytes())
    }
    
    // Write metadata key
    localCmd := makeSetCommand(metaKey, cmd.exptime, METADATA_SIZE)
    err = setLocal(localWriter, localCmd, nil, metaDataBuf.Bytes())
    if err != nil { return err }
    
    // Read server's response
    // TODO: Error handling of ERROR response
    response, err := localReader.ReadString('\n')
    
    if verbose { fmt.Println(response) }
    
    // Write all the data chunks
    // TODO: Clean up if a data chunk write fails
    // Failure can mean the write failing at the I/O level
    // or at the memcached level, e.g. response == ERROR
    for i := 0; i < numChunks; i++ {
        // Build this chunk's key
        key := makeChunkKey(cmd.key, i)
        
        if verbose { fmt.Println(key) }
        
        // indices for slicing, end exclusive
        start, end := sliceIndices(i, cmd.length)
        
        chunkBuf := buf[start:end]
        
        // Pad the data to always be CHUNK_SIZE
        if (end-start) < CHUNK_SIZE {
            padding := CHUNK_SIZE - (end-start)
            padtext := bytes.Repeat([]byte{byte(0)}, padding)
            chunkBuf = append(chunkBuf, padtext...)
        }
        
        // Write the key
        localCmd = makeSetCommand(key, cmd.exptime, FULL_DATA_SIZE)
        err = setLocal(localWriter, localCmd, token, chunkBuf)
        if err != nil { return err }
        
        // Read server's response
        // TODO: Error handling of ERROR response from memcached
        response, _ := localReader.ReadString('\n')
        
        if verbose { fmt.Println(response) }
    }
    
    return Response{}
}

func handleGet(cmd GetCmdLine, localReader *bufio.Reader, localWriter *bufio.Writer) (chan GetResponse, chan error) {
    // No buffering here so there's not multiple gets in memory
    dataOut = make(chan GetResponse)
    errorOut = make(chan error)
    go realHandleGet(cmd, dataOut, errorOut, localReader, localWriter)
    return dataOut, errorOut
}

func realHandleGet(cmd GetCmdLine, dataOut chan GetResponse, errorOut chan error,
                   localReader *bufio.Reader, localWriter *bufio.Writer) {
    // read index
    // make buf
    // for numChunks do
    //   read chunk, append to buffer
    // send response
        
    outer: for _, key := range cmd.keys {
        _, metaData, err := getMetadata(localReader, localWriter, key)
        if err != nil {
            if err == MISS {
                if verbose { fmt.Println("Get miss because of missing metadata. Key:", key) }
                continue outer
            }
            
            errorOut <- err
            close(errorOut)
            close(dataOut)
            return
        }
        
        // Retrieve all the data from memcached
        dataBuf := make([]byte, metaData.Length)
        tokenBuf := make([]byte, 16)
        
        for i := 0; i < int(metaData.NumChunks); i++ {
            if verbose { fmt.Println("CHUNK", i) }
            chunkKey := makeChunkKey(key, i)
            
            // indices for slicing, end exclusive
            // TODO: pass chunk size
            start, end := sliceIndices(i, int(metaData.Length))
            
            if verbose { fmt.Println("start:", start, "| end:", end) }
            
            // Get the data directly into our buf
            chunkBuf := dataBuf[start:end]
            getCmd := makeGetCommand(chunkKey)
            err = getLocalIntoBuf(localReader, localWriter, getCmd, tokenBuf, chunkBuf)
            
            if err != nil {
                if err == MISS {
                    if verbose { fmt.Println("Get miss because of missing chunk. Cmd:", getCmd) }
                    continue outer
                }
                
                errorOut <- err
                close(errorOut)
                close(dataOut)
                return
            }
            
            if (!bytes.Equal(metaData.Token[:], tokenBuf)) {
                if verbose { fmt.Println("Get miss because of invalid chunk token. Cmd:", getCmd) }
                continue outer
            }
        }
        
        dataOut <- GetResponse {
            Metadata: metaData,
            Data:     dataBuf,
        }
    }
    
    close(dataOut)
    close(errorOut)
}

func handleDelete(cmd DeleteCmdLine, localReader *bufio.Reader, localWriter *bufio.Writer) error {
    // read metadata
    // delete metadata
    // for 0 to metadata.numChunks
    //  delete item
    
    metaKey, metaData, err := getMetadata(localReader, localWriter, cmd.key)
    
    if err != nil {
        if err == MISS {
            if verbose { fmt.Println("Delete miss because of missing metadata. Key:", cmd.key) }
            return NOT_FOUND
        }
        return err
    }
    
    err = deleteLocal(localReader, localWriter, metaKey)
    if err != nil { return err }
    
    for i := 0; i < int(metaData.NumChunks); i++ {
        chunkKey := makeChunkKey(cmd.key, i)
        err := deleteLocal(localReader, localWriter, chunkKey)
        if err != nil { return err }
    }
    
    return nil
}

func handleTouch(cmd TouchCmdLine, localReader *bufio.Reader, localWriter *bufio.Writer) error {
    // read metadata
    // for 0 to metadata.numChunks
    //  touch item
    // touch metadata
    
    metaKey, metaData, err := getMetadata(localReader, localWriter, cmd.key)
        
    if err != nil {
        if err == MISS {
            if verbose { fmt.Println("Touch miss because of missing metadata. Key:", cmd.key) }
            return NOT_FOUND
        }
        
        return err
    }
    
    for i := 0; i < int(metaData.NumChunks); i++ {
        chunkKey := makeChunkKey(cmd.key, i)
        err := touchLocal(localReader, localWriter, chunkKey, cmd.exptime)
        if err != nil { return err }
    }
    
    err = touchLocal(localReader, localWriter, metaKey, cmd.exptime)
    if err != nil { return err }
    
    return nil
}
