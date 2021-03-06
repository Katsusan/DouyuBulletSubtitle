package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"time"

	"./data"
	"./db"
)

const (
	SERVER_PROTOCOL string = "tcp4"
	BARRAGE_SERVER  string = "openbarrage.douyutv.com"
	SERVER_PORT     string = "8601"
	TABLE_PREFIX    string = "barrage_"
)

var (
	ErrDataTruncated = errors.New("Not a full server response")
	tcpconn          *net.TCPConn
	mysqlcfg         = new(data.MysqlConfig)
	roomid           *string
	dbOK             bool = false
)

func init() {
	log.Printf("main.init exec.\n")
	roomid = flag.String("rid", "9999", "room id.")
	flag.StringVar(&mysqlcfg.Username, "u", "root", "username used for connecting MySQL")
	flag.StringVar(&mysqlcfg.Password, "p", "", "password used for authorizing")
	flag.StringVar(&mysqlcfg.Host, "h", "127.0.0.1", "given host for connection")
	flag.StringVar(&mysqlcfg.Port, "port", "3306", "tcp/ip port for connection")
	flag.StringVar(&mysqlcfg.DbName, "d", "dybarrage", "database for storing barrages")
	flag.StringVar(&mysqlcfg.CharSet, "c", "utf8", "character set for connecting Mysql")
	mysqlcfg.TableID = *roomid

	flag.Parse()

	//only parameter count > 2 means will store barrages into DB
	if flag.NFlag() > 1 {
		//go DB initialization
		err := db.InitEngine(mysqlcfg)

		if err != nil {
			log.Printf("DB initializing failed. And will not write barrages into DB\n")
		} else {
			//otherwise means DB connect OK
			dbOK = true
		}
	}

}

func main() {
	initconn()
	readresponse(tcpconn)
	defer func() {
		logout()
		tcpconn.Close()
	}()
}

//initconn will do some initializing operations, including:
//	1. connect to barrage server(openbarrage.douyutv.com:8601) via TCP protocol
//	2. make log-in request to barrage server
//	3. if log-in succeeds, then will receive correspond message from barrage server.
//	4. send barrage group request to barrage server. (due to huge amounts of barrages, barrage grouping is necessary)
//	5. server will add client to specified barrage group after receiving barrage group request.
func initconn() {
	//step1. connect to server
	tcpaddr, err := net.ResolveTCPAddr(SERVER_PROTOCOL, BARRAGE_SERVER+":"+SERVER_PORT)
	checkErr(err)

	tcpconn, err = net.DialTCP(SERVER_PROTOCOL, nil, tcpaddr)
	checkErr(err)

	//log.Println("TCP connect ok.")

	//step2. send log-in request
	loginreq := []byte("type@=loginreq" + "/roomid@=" + *roomid + "/")
	sendmsg(tcpconn, loginreq)

	//step3. server response
	//server do not check the user auth, so moves on

	//step4. send barrage group request to server
	groupreq := []byte("type@=joingroup/rid@=" + *roomid + "/gid@=-9999/")
	sendmsg(tcpconn, groupreq)
	go keeplive()
}

func sendmsg(tc *net.TCPConn, b []byte) {
	msglen := len(b) + 8 + 1
	msgtype := 689
	var (
		msglenbuf  [4]byte
		msgtypebuf [4]byte
	)
	binary.LittleEndian.PutUint32(msglenbuf[:], uint32(msglen))
	binary.LittleEndian.PutUint32(msgtypebuf[:], uint32(msgtype))

	msghead := append(append(msglenbuf[:], msglenbuf[:]...), msgtypebuf[:]...)
	msgall := append(append(msghead, b...), 0x00)
	_, err := tc.Write(msgall)
	//log.Printf("sent->%s\n", msgall)
	checkErr(err)

	/*
		for sent := 0; sent < len(b); {
			sgst, err := tc.Write(b)
			log.Printf("sent->%s\n", b)
			checkErr(err)
			sent = sent + sgst
		}*/

}

func readresponse(tc *net.TCPConn) ([]byte, error) {
	result := bytes.NewBuffer(nil)
	var buf [1024]byte

	for {
		n, err := tc.Read(buf[0:])
		result.Write(buf[0:n])
		//log.Printf("buf[0:n]->%s\n", buf[0:n])
		respondtoserver(buf[0:n])
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
	}
	//log.Printf("response->%s\n", result.Bytes())
	return result.Bytes(), nil
}

func respondtoserver(fres []byte) error {
	termindbytes := []byte{'/', 0x00}
	if !bytes.Contains(fres, termindbytes) {
		return ErrDataTruncated
	}

	resgroup := bytes.SplitN(fres, termindbytes, 100)
	for i := 0; i < len(resgroup)-1; i++ {
		//log.Printf("resgroup[%d]->%s\n", i, resgroup[i])
		responsHandle(resgroup[i])
	}

	return nil

}

func responsHandle(serverres []byte) error {
	retype := regexp.MustCompile("type@=([a-z]+)")
	respg := retype.FindSubmatch(serverres) // respg[0]->type@=pingreq, respg[1]->pingreq
	if respg == nil {
		//log.Printf("Not correct server response(no type@= flag): %s\n", serverres)
		return ErrDataTruncated
	}

	renickname := regexp.MustCompile("nn@=(.+?)/")
	rechatmsg := regexp.MustCompile("txt@=(.+?)/")
	relevel := regexp.MustCompile("level@=([0-9]+?)/")
	relivestat := regexp.MustCompile("live_stat@=([0-9]+?)/")

	switch {
	case bytes.Equal(respg[1], []byte("loginres")):
		//log-in response, not necessary to respond
		//log.Println("get loginres msg")
		livestat := relivestat.FindSubmatch(serverres)[1]
		if bytes.Equal(livestat, []byte("0")) { //live_stat seems not work.(always 0 whatever streaming or not)
			//log.Fatal("主播不在直播。\n")
		}
		//log.Printf("loginres-> %s\n", serverres)
	case bytes.Equal(respg[1], []byte("keeplive")):
		//keeplive msg comes:type@=keeplive/tick@=1345465467
		keephead := []byte("type@=keeplive/tick@=")
		tn := strconv.AppendInt(keephead, time.Now().Unix(), 10)
		livemsg := append(tn, []byte("/\\0")...)
		sendmsg(tcpconn, livemsg)

	case bytes.Equal(respg[1], []byte("pingreq")):
		//ping msg comes:type@=pingreq/tick@=15414126085050 -> unknown response, don't respond
		/*retick := regexp.MustCompile("tick@=([0-9]+)")
		typetick := retick.FindSubmatch(serverres)
		if typetick == nil {
			//rarely happens
			return
		}
		respmsg := append(append([]byte("type@=pingresp/tick@="), typetick[1]...), '/')*/

	case bytes.Equal(respg[1], []byte("uenter")):
		//msg of user's entering room, eg: type@=uenter/rid@=97376/uid@=11880384/nn@=uux/level@=22
		nickname := renickname.FindSubmatch(serverres)[1]
		userlevel := relevel.FindSubmatch(serverres)[1]
		fmt.Printf("欢迎:%s(level:%s)来到直播间\n", nickname, userlevel)

	case bytes.Equal(respg[1], []byte("chatmsg")):
		nickname := renickname.FindSubmatch(serverres)[1]
		chatmsg := rechatmsg.FindSubmatch(serverres)[1]
		fmt.Printf("%s: %s\n", nickname, chatmsg)
		if dbOK == true {
			affected, err := db.MainDB.Insert(&data.Barrage{Nickname: string(nickname), Chatmsg: string(chatmsg), Roomid: *roomid})
			log.Printf("affected: %d, error: %v\n", affected, err)
			if err != nil {
				log.Printf("Barrage insert failed, %s: %s\n", nickname, chatmsg)
			}
		}
	default:
		//barrage and other message(such as gift.)
		//log.Printf("message->%s\n", serverres)
	}

	return nil
}

//keeplive executed only after TCP connection established and loginreq/joingroup was sent
func keeplive() {
	//msg format: type@=keeplive/tick@=1541401463/
	//keephead := []byte("type@=keeplive/tick@=") -> old format,depreciated
	keepmsg := []byte("type@=mrkl/")
	if true {
		for {
			sendmsg(tcpconn, keepmsg)
			time.Sleep(45 * time.Second)
		}
	}

}

func initdb() {

}

func logout() {
	logoutmsg := []byte("type@=logout/")
	sendmsg(tcpconn, logoutmsg)
}
func checkErr(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
