package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"gopkg.in/ldap.v2"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"time"
	"html/template"
	"github.com/Symantec/keymaster/lib/authutil"
	"github.com/cviecco/go-simple-oidc-auth/authhandler"
	"net/http"
	"encoding/json"
	"sort"
	"fmt"
	"database/sql"
	_"github.com/mattn/go-sqlite3"
	"net/smtp"
	"github.com/mssola/user_agent"
	texttemplate "text/template"
)



type baseConfig struct {
	HttpAddress           string `yaml:"http_address"`
	TLSCertFilename       string `yaml:"tls_cert_filename"`
	TLSKeyFilename        string `yaml:"tls_key_filename"`
	StorageURL            string `yaml:"storage_url"`
	OpenIDCConfigFilename string `yaml:"openidc_config_filename"`
}

type UserInfoLDAPSource struct {
	BindUsername       string `yaml:"bind_username"`
	BindPassword       string `yaml:"bind_password"`
	LDAPTargetURLs     string `yaml:"ldap_target_urls"`
	UserSearchBaseDNs  string `yaml:"user_search_base_dns"`
	UserSearchFilter   string `yaml:"user_search_filter"`
	GroupSearchBaseDNs string `yaml:"group_search_base_dns"`
	GroupSearchFilter  string `yaml:"group_search_filter"`
	Admins 			   string `yaml:"super_admins"`
}


type AppConfigFile struct {
	Base       baseConfig         `yaml:"base"`
	SourceLDAP UserInfoLDAPSource `yaml:"source_config"`
	TargetLDAP UserInfoLDAPSource `yaml:"target_config"`
}


type RuntimeState struct {
	Config      AppConfigFile
	source_ldap *ldap.Conn
	target_ldap *ldap.Conn
	dbType string
	db *sql.DB
}

type GetGroups struct{
	AllGroups []string `json:"allgroups"`

}

type GetUsers struct{
	Users []string `json:"Users"`
}

type GetUserGroups struct{
	UserName string `json:"Username"`
	UserGroups []string `json:"usergroups"`
}

type GetGroupUsers struct{
	GroupName string `json:"groupname"`
	Groupusers []string `json:"Groupusers"`

}

type Response struct{
	UserName string
	Groups []string
	Users []string
	Pending_actions [][]string
}


type group_info struct {
	groupname string
	description string
	memberUid []string
	member []string
	cn string
}

const ldapTimeoutSecs = 10

//maximum possible paging size number
const maximum_pagingsize = 2147483647


var nsaccount_lock = []string{"True"}

var (
	configFilename = flag.String("config", "config.yml", "The filename of the configuration")
	//tpl *template.Template
	//debug          = flag.Bool("debug", false, "enable debugging output")
	authSource     *authhandler.SimpleOIDCAuth
)

//Initialsing database
func initDB(state *RuntimeState) (err error) {

	state.dbType = "sqlite3"
	state.db, err = sql.Open("sqlite3", "./ldap-group-management1.db")
	if err != nil {
		return err
	}
	if true {
		sqlStmt := `create table if not exists pending_requests (username text not null, groupname text not null, time_stamp int not null);`
		_, err = state.db.Exec(sqlStmt)
		if err != nil {
			log.Printf("init sqlite3 err: %s: %q\n", err, sqlStmt)
			return err
		}
	}

	return nil
}



//insert a request into DB
func (state *RuntimeState) insertInDB(username string,groupname []string) error {

	stmtText := "insert into pending_requests(username, groupname, time_stamp) values (?,?,?)"
	stmt, err := state.db.Prepare(stmtText)
	if err != nil {
		log.Print("Error Preparing statement")
		log.Fatal(err)
	}
	defer stmt.Close()
	for _, entry := range groupname {
		if state.entryExistsorNot(username, entry) || state.isGroupMemberorNot(entry,username){
			continue
		} else {

			_, err = stmt.Exec(username, entry, time.Now().Unix())
			if err != nil {
				return err
			}
		}
	}
	return nil
}

//delete the request after approved or declined
func (state *RuntimeState) deleteEntryInDB(username string,groupname string) error{

	stmtText :="delete from pending_requests where username= ? and groupname= ?;"
	stmt, err := state.db.Prepare(stmtText)
	if err != nil {
		log.Print("Error Preparing statement")
		log.Fatal(err)
	}
	defer stmt.Close()
	_, err = stmt.Exec(username,groupname)
	if err != nil {
		return err
	}
	return nil

}

//deleting all groups in DB which are deleted from Target LDAP
func (state *RuntimeState) deleteEntryofGroupsInDB(groupnames []string) error {

	stmtText := "delete from pending_requests where groupname= ?;"
	stmt, err := state.db.Prepare(stmtText)
	if err != nil {
		log.Print("Error Preparing statement")
		return err
	}
	defer stmt.Close()
	for _, entry := range groupnames {
		_, err = stmt.Exec(entry)
		if err != nil {
			return err
		}
	}
	return nil

}

//Search for a particular request made by a user (or) a group. (for my_pending_actions)
func (state *RuntimeState) findrequestsofUserinDB(username string) ([]string,bool,error) {
	stmtText:="select groupname from pending_requests where username=?;"
	stmt,err:=state.db.Prepare(stmtText)
	if err != nil {
		log.Print("Error Preparing statement")
		log.Fatal(err)
	}
	defer stmt.Close()
	var groupname []string
	rows,err := stmt.Query(username)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			log.Printf("err='%s'", err)
			defer rows.Close()
			return nil,false,nil
		} else {
			log.Printf("Problem with db ='%s'", err)
			defer rows.Close()
			return nil,false, err
		}
	}
	for rows.Next(){
		var group_Name string
		err=rows.Scan(&group_Name)
		groupname=append(groupname,group_Name)
	}
	defer rows.Close()

	return groupname,true, nil

}

//looks in the DB if the entry already exists or not
func (state *RuntimeState) entryExistsorNot(username string,groupname string)bool{
	stmt_Text:="select * from pending_requests where username=? and groupname=?;"
	stmt,err:=state.db.Prepare(stmt_Text)
	if err != nil {
		log.Print("Error Preparing statement")
		log.Fatal(err)
	}
	defer stmt.Close()
	rows,err:=stmt.Query(username,groupname)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			log.Printf("err='%s'", err)
			return false
		} else {
			log.Printf("Problem with db ='%s'", err)
			return false
		}
	}
	if rows.Next() {
		defer rows.Close()
		return true
	}
	defer rows.Close()
	return false
}


//(username,groupname) get whole db entries.
func (state *RuntimeState) getDB_entries()([][]string,error){
	stmtText:="select username,groupname from pending_requests;"
	stmt,err:=state.db.Prepare(stmtText)
	if err != nil {
		log.Print("Error Preparing statement")
		log.Fatal(err)
	}
	defer stmt.Close()
	var entry [][]string
	rows,err:=stmt.Query()
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			log.Printf("err='%s'", err)
			defer rows.Close()
			return nil,err
		} else {
			log.Printf("Problem with db ='%s'", err)
			defer rows.Close()
			return nil,err
		}
	}
	var each_entry1 string
	var each_entry2 string
	for rows.Next(){
		err=rows.Scan(&each_entry1,&each_entry2)
		var each_entry =[]string{each_entry1,each_entry2}
		entry=append(entry,each_entry)
	}
	defer rows.Close()
	return entry,nil
}

//parses the config file
func loadConfig(configFilename string) (RuntimeState, error) {

	var state RuntimeState

	if _, err := os.Stat(configFilename); os.IsNotExist(err) {
		err = errors.New("mising config file failure")
		return state, err
	}

	//ioutil.ReadFile returns a byte slice (i.e)(source)
	source, err := ioutil.ReadFile(configFilename)
	if err != nil {
		err = errors.New("cannot read config file")
		return state, err
	}

	//Unmarshall(source []byte,out interface{})decodes the source byte slice/value and puts them in out.
	err = yaml.Unmarshal(source, &state.Config)

	if err != nil {
		err = errors.New("Cannot parse config file")
		log.Printf("Source=%s", source)
		return state, err
	}
	err = initDB(&state)
	if err != nil {
		return state, err
	}
	return state, err
}



//Establishing connection
func GetLDAPConnection(u url.URL, timeoutSecs uint, rootCAs *x509.CertPool) (*ldap.Conn, string, error) {

	if u.Scheme != "ldaps" {
		err := errors.New("Invalid ldap scheme (we only support ldaps)")
		return nil, "", err
	}

	serverPort := strings.Split(u.Host, ":")
	port := "636"

	if len(serverPort) == 2 {
		port = serverPort[1]
	}

	server := serverPort[0]
	hostnamePort := server + ":" + port

	timeout := time.Duration(time.Duration(timeoutSecs) * time.Second)
	start := time.Now()

	tlsConn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", hostnamePort, &tls.Config{ServerName: server, RootCAs: rootCAs, InsecureSkipVerify: true})

	if err != nil {
		log.Printf("rooCAs=%+v,  serverName=%s, hostnameport=%s, tlsConn=%+v", rootCAs, server, hostnamePort, tlsConn)
		errorTime := time.Since(start).Seconds() * 1000
		log.Printf("connection failure for:%s (%s)(time(ms)=%v)", server, err.Error(), errorTime)
		return nil, "", err
	}

	// we dont close the tls connection directly  close defer to the new ldap connection
	conn := ldap.NewConn(tlsConn, true)
	return conn, server, nil
}



//Function which returns the array of disabled accounts from Source LDAP.--required
func (state *RuntimeState) DisabledAccountsinSourceLDAP(UserSearchBaseDNs string, UserSearchFilter string, Attributes []string) ([]string, error) {
	var disabled_accounts []string

	searchrequest := ldap.NewSearchRequest(UserSearchBaseDNs, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, UserSearchFilter, Attributes, nil)

	result, err := state.source_ldap.SearchWithPaging(searchrequest, maximum_pagingsize)
	if err != nil {
		return nil, err
	}
	if len(result.Entries) == 0 {
		log.Println("No records found")
	}
	for _, entry := range result.Entries {
		cname := entry.GetAttributeValue("sAMAccountName")
		disabled_accounts = append(disabled_accounts, strings.ToLower(cname))
	}
	return disabled_accounts, nil
}



//function which compares the users disabled accounts in Source LDAP and Target LDAP and adds the attribute nsaccountLock in TARGET LDAP for the disbaled USer.
//---required
func (state *RuntimeState) CompareDisabledaccounts(result []string) error {
	for _, entry := range result {
		entry = state.Create_UserDN(entry)

		modify := ldap.NewModifyRequest(entry)
		modify.Replace("nsaccountLock", nsaccount_lock)
		err := state.target_ldap.Modify(modify)
		if err != nil {
			return err
		}
	}
	return nil

}



//Get all ldap users and put that in map ---required
func (state *RuntimeState) GetallUsers(UserSearchBaseDNs string, UserSearchFilter string, Attributes []string) (map[string]string, error) {
	AllUsersinLdap := make(map[string]string)

	searchrequest := ldap.NewSearchRequest(UserSearchBaseDNs, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, UserSearchFilter, Attributes, nil)
	result, err := state.target_ldap.Search(searchrequest)
	if err != nil {
		return nil, err
	}

	if len(result.Entries) == 0 {
		log.Println("No records found")
	}
	for _, entry := range result.Entries {
		uid := entry.GetAttributeValue("uid")
		AllUsersinLdap[uid] = uid
	}

	return AllUsersinLdap, nil
}



//find out which accounts need to be locked in Target ldap(i.e. which accounts needs attribute nsaccountLock=True) --required
func FindLockAccountsinTargetLdap(TargetLDAP_Users map[string]string, LockedAccounts_SourceLDAP []string) ([]string, error) {
	var lock_accounts []string
	for _, entry := range LockedAccounts_SourceLDAP {
		if entry, ok := TargetLDAP_Users[entry]; ok {

			lock_accounts = append(lock_accounts, entry)
		}

	}
	return lock_accounts, nil
}



//To build a user base DN using uid only for Target LDAP.
func (state *RuntimeState) Create_UserDN(username string) string {
	//uid := username
	result := "uid=" + username + "," +state.Config.TargetLDAP.UserSearchBaseDNs

	return string(result)

}



//To build a GroupDN for a particular group in Target ldap
func (state *RuntimeState) Create_GroupDN(groupname string) string {
	result := "cn=" + groupname + "," + state.Config.TargetLDAP.GroupSearchBaseDNs

	return string(result)

}

//Creating a Group --required
func (state *RuntimeState) create_Group(groupinfo group_info) error{
	entry:=state.Create_GroupDN(groupinfo.groupname)
	group:=ldap.NewAddRequest(entry)
	group.Attribute("objectClass",[]string{"posixGroup","top","groupOfNames"})
	group.Attribute("cn",[]string{groupinfo.groupname})
	group.Attribute("description",[]string{groupinfo.description})
	group.Attribute("member",groupinfo.member)
	group.Attribute("memberUid",groupinfo.memberUid)
	group.Attribute("gidNumber",[]string{"20000"})
	err:=state.target_ldap.Add(group)
	if err!=nil{
		return err
	}
	return nil
}

// POST
// Create a group handler --required
func (state *RuntimeState) createGroup_handler(w http.ResponseWriter,r *http.Request){
	userInfo, err := authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	//vals:=r.URL.Query()
	username:=*userInfo.Username
	if state.userisAdminOrNot(username){
		err := r.ParseForm()
		if err != nil {
			panic("Cannot parse form")
		}
		var groupinfo group_info
		groupinfo.groupname = r.PostFormValue("groupname")
		groupinfo.description = r.PostFormValue("description")
		members := r.PostFormValue("members")
		for _, member := range strings.Split(members, ",") {
			groupinfo.memberUid = append(groupinfo.memberUid, member)
			groupinfo.member = append(groupinfo.member, state.Create_UserDN(member))
		}
		err = state.create_Group(groupinfo)
		if err != nil {
			http.Error(w,"error occurred! May be group name exists or may be members are not available!",http.StatusInternalServerError)
			log.Print(err)
		}
		if state.userisAdminOrNot(username) == true {
			generateHTML(w, Response{UserName:username}, "index","admins_sidebar", "groupcreation_success")

		} else {
			generateHTML(w, Response{UserName:username}, "index","sidebar","groupcreation_success")
		}
	} else {
		http.NotFoundHandler()
	}
}



//Delete groups handler --required
func (state *RuntimeState) deleteGroup_handler(w http.ResponseWriter,r *http.Request){
	userInfo, err := authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	//vals:=r.URL.Query()
	username:=*userInfo.Username
	if state.userisAdminOrNot(username) {
		err := r.ParseForm()
		if err != nil {
			panic("Cannot parse form")
		}
		var groupnames []string
		groups := r.PostFormValue("groupnames")
		for _, eachGroup := range strings.Split(groups, ",") {
			groupnames = append(groupnames, eachGroup)
		}
		err = state.delete_Group(groupnames)
		if err != nil {
			http.Error(w,"error occurred! May be there is no such group!",http.StatusInternalServerError)
			log.Print(err)
		}
		err=state.deleteEntryofGroupsInDB(groupnames)
		if err!=nil{
			panic(err)
		}
		if state.userisAdminOrNot(username) == true {
			generateHTML(w, Response{UserName:username}, "index","admins_sidebar", "groupdeletion_success")

		} else {
			generateHTML(w, Response{UserName:username}, "index","sidebar","groupdeletion_success")
		}
	} else{
		http.NotFoundHandler()
	}
}

//deleting a Group from target ldap. --required
func (state *RuntimeState) delete_Group(groupnames []string) error{
	for _,entry:=range groupnames {
		group_dn:=state.Create_GroupDN(entry)

		del_req := ldap.NewDelRequest(group_dn,nil)
		err:=state.target_ldap.Del(del_req)
		if(err!=nil){
			return err
		}

	}
	return nil
}


//Adding an attritube called 'description' to a dn in Target Ldap --required
func (state *RuntimeState) addAtributeDescription(groupname string)error{

	entry:=state.Create_GroupDN(groupname)
	modify := ldap.NewModifyRequest(entry)
	modify.Delete("description", []string{"self-managed"})

	//modify.Add("description", []string{"created by me"})
	err := state.target_ldap.Modify(modify)
	if err != nil {
		return err
	}
	return nil

}

//Deleting the attribute in a dn in Target Ldap. --required
func (state *RuntimeState) deleteDescription(result []string) error {
	for _, entry := range result {
		entry = state.Create_GroupDN(entry)

		modify := ldap.NewModifyRequest(entry)

		modify.Delete("description", []string{"created by Midpoint"})
		err := state.target_ldap.Modify(modify)
		if err != nil {
			return err
		}
	}
	return nil
}


//function to get details of a user from Target ldap.(should make some changes) --required
func (state *RuntimeState) UserInfo(User_dn string) ([]string, error) {
	var user_info []string
	searchrequest := ldap.NewSearchRequest(User_dn, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, "((objectClass=*))", nil, nil)
	result, err := state.target_ldap.Search(searchrequest)
	if err != nil {
		return nil, err
	}
	for _, entry := range result.Entries {
		user_info = entry.GetAttributeValues("objectClass")
		//println(entry.GetAttributeValue(entry.Attributes[5].Name))
	}
	return user_info, nil
}



//function to get all the groups in Target ldap and put it in array --required
func (state *RuntimeState) get_allGroups(Group_dn string) ([]string, error) {
	var All_Groups []string
	searchrequest := ldap.NewSearchRequest(Group_dn, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, "(&(objectClass=posixGroup)(objectClass=groupofNames)(objectClass=top))", []string{"cn"}, nil)
	result, err := state.target_ldap.Search(searchrequest)
	if err != nil {
		return nil, err
	}
	for _, entry := range result.Entries {
		All_Groups = append(All_Groups, entry.GetAttributeValue("cn"))
	}
	return All_Groups, nil
}



// GetGroupsOfUser returns the all groups of a user. --required
func (state *RuntimeState) getGroupsOfUser(groupdn string, username string) ([]string, error) {
	Base := groupdn
	searchRequest := ldap.NewSearchRequest(
		Base,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(&(memberUid="+username+" ))",
		[]string{"cn"}, //memberOf (if searching other way around using usersdn instead of groupdn)
		nil,
	)
	sr, err := state.target_ldap.Search(searchRequest)
	if err != nil {
		return nil, err
	}
	groups := []string{}
	for _, entry := range sr.Entries {
		groups = append(groups, entry.GetAttributeValue("cn"))
	}
	return groups, nil
}



//returns all the users of a group --required
func (state *RuntimeState) getUsersofaGroup(groupname string) ([][]string, error) {
	Base := state.Create_GroupDN(groupname)

	searchRequest := ldap.NewSearchRequest(
		Base,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"((objectClass=*))",
		[]string{"memberUid"},
		nil,
	)
	sr, err := state.target_ldap.Search(searchRequest)
	if err != nil {
		return nil, err
	}
	users := [][]string{}
	for _, entry := range sr.Entries {
		users = append(users, entry.GetAttributeValues("memberUid"))
	}
	return users, nil
}




// parse HTML templates and pass in a list of file names, and get a template

func parseTemplateFiles(filenames ...string) (t *template.Template) {
	var files []string
	t = template.New("index")
	for _, file := range filenames {
		files = append(files, fmt.Sprintf("templates/%s.html", file))
	}
	t = template.Must(t.ParseFiles(files...))
	return
}



func generateHTML(w http.ResponseWriter, data interface{}, filenames ...string) {
	var files []string
	for _, file := range filenames {
		files = append(files, fmt.Sprintf("templates/%s.html", file))
	}

	templates := template.Must(template.ParseFiles(files...))
	templates.ExecuteTemplate(w, "index", data)
}

//parse super admins of Target Ldap
func (state *RuntimeState) parseSuperAdmins()([]string){
	var superAdminsInfo []string
	for _, admin := range strings.Split(state.Config.TargetLDAP.Admins, ",") {
		fmt.Print(admin)
		superAdminsInfo = append(superAdminsInfo,admin)
	}
	return superAdminsInfo
}

//if user is super admin or not
func (state *RuntimeState) userisAdminOrNot(username string)(bool){
	superAdmins:=state.parseSuperAdmins()
	fmt.Print(superAdmins)
	for _,user:=range superAdmins{
		if user==username{
			return true
		}
	}
	return false
}

//if groupname already exists or not

//All handlers and API endpoints starts from here.

//Display all groups in Target LDAP --required
func (state *RuntimeState) GetallGroups_Handler(w http.ResponseWriter, r *http.Request) {
	var AllGroups_TargetLdap GetGroups

	Allgroups, err := state.get_allGroups(state.Config.TargetLDAP.GroupSearchBaseDNs)

	if err != nil {
		log.Panic(err)
	}
	sort.Strings(Allgroups)
	AllGroups_TargetLdap.AllGroups = Allgroups
	json.NewEncoder(w).Encode(AllGroups_TargetLdap)

}




//Display all users in Target LDAP --required
func (state *RuntimeState) GetallUsers_Handler(w http.ResponseWriter, r *http.Request) {
	var AllUsers_TargetLdap GetUsers

	AllUsers, err := state.GetallUsers(state.Config.TargetLDAP.UserSearchBaseDNs, state.Config.TargetLDAP.UserSearchFilter, []string{"uid"})

	if err != nil {
		log.Println(err)
	}

	for k := range AllUsers {
		AllUsers_TargetLdap.Users = append(AllUsers_TargetLdap.Users, k)
	}

	json.NewEncoder(w).Encode(AllUsers_TargetLdap)
}




//Displays all Groups of a User. --required
func (state *RuntimeState) GetGroupsofUser_Handler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	params, ok := q["username"]
	if ok {

		var user_groups GetUserGroups

		user_groups.UserName = params[0] //username is "cn" Attribute of a User
		UsersAllgroups, err := state.getGroupsOfUser(state.Config.TargetLDAP.GroupSearchBaseDNs, user_groups.UserName)
		sort.Strings(UsersAllgroups)
		user_groups.UserGroups = UsersAllgroups

		if err != nil {
			log.Println(err)
		}

		json.NewEncoder(w).Encode(user_groups)
	}
	http.NotFoundHandler()
}



//Displays All Users in a Group --required
func (state *RuntimeState) GetUsersinGroup_Handler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	params, ok := q["groupname"]
	if ok {
		var group_users GetGroupUsers

		group_users.GroupName = params[0] //username is "cn" Attribute of a User
		AllUsersinGroup, err := state.getUsersofaGroup(group_users.GroupName)
		sort.Strings(AllUsersinGroup[0])
		group_users.Groupusers = AllUsersinGroup[0]

		if err != nil {
			log.Println(err)
		}

		json.NewEncoder(w).Encode(group_users)
	}
	http.NotFoundHandler()
}


//Main page with all LDAP groups displayed
func  (state *RuntimeState) Index_Handler(w http.ResponseWriter, r *http.Request) {
	userInfo, err := authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	Allgroups, err := state.get_allGroups(state.Config.TargetLDAP.GroupSearchBaseDNs)

	if err != nil {
		log.Panic(err)
	}
	sort.Strings(Allgroups)
	user := *userInfo.Username
	response := Response{user, Allgroups, nil,nil}
	//response.UserName=*userInfo.Username
	if (state.userisAdminOrNot(user)==true) {
		generateHTML(w,response,"index","admins_sidebar","groups")

	} else {
		generateHTML(w, response, "index", "sidebar","groups")
	}
}


//it helps to findout the current maximum gid number in ldap.
func (state *RuntimeState) current_maximumGidNumber()(string,error){
	searchRequest := ldap.NewSearchRequest(
		state.Config.TargetLDAP.GroupSearchBaseDNs,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"(&(objectClass=posixGroup)(objectClass=top)(objectClass=groupOfNames))",
		[]string{"gidNumber"},
		nil,
	)
	sr, err := state.target_ldap.Search(searchRequest)
	if err != nil {
		return "error in ldapsearch", err
	}
	var max="0"
	for _,entry:=range sr.Entries{
		gidnum:=entry.GetAttributeValue("gidNumber")
		value:=gidnum
		if value>max {
			max=value
		}
	}
	fmt.Println(max)
	return max,nil
}
//Group page.
func (state *RuntimeState) Group_Handler(w http.ResponseWriter, r *http.Request){
	vals := r.URL.Query()

	generateHTML(w,vals.Get("groupname"),"index","sidebar","group_info")

}

//User Groups page
func (state *RuntimeState) MyGroups_Handler(w http.ResponseWriter,r *http.Request){
	userInfo, err := authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	user_groups,err:=state.getGroupsOfUser(state.Config.TargetLDAP.GroupSearchBaseDNs,username)
	if(err!=nil){
		log.Println(err)
	}
	sort.Strings(user_groups)
	response:=Response{username,user_groups,nil,nil}
	if state.userisAdminOrNot(response.UserName) {
		generateHTML(w, response, "index", "admins_sidebar", "my_groups")
	} else{
		generateHTML(w,response,"index","sidebar","my_groups")
	}
}


//user's pending requests
func (state *RuntimeState) pending_Requests(w http.ResponseWriter,r *http.Request) {
	userInfo, err := authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	groupnames, _, _ := state.findrequestsofUserinDB(username)
	response := Response{UserName: username, Groups: groupnames, Users: nil,Pending_actions:nil}
	if groupnames == nil {
		if state.userisAdminOrNot(username) == true {
			generateHTML(w, response, "index", "admins_sidebar", "no_pending_requests")

		} else {
			generateHTML(w, response, "index", "sidebar", "no_pending_requests")
		}
	} else {
		if state.userisAdminOrNot(username) == true {
			generateHTML(w, response, "index", "admins_sidebar", "pending_requests")

		} else {
			generateHTML(w, response, "index", "sidebar", "pending_requests")
		}
	}
}


func (state *RuntimeState) creategroup_WebpageHandler(w http.ResponseWriter, r *http.Request){
	userInfo, err := authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	Allgroups, err := state.get_allGroups(state.Config.TargetLDAP.GroupSearchBaseDNs)

	if err != nil {
		log.Panic(err)
	}
	sort.Strings(Allgroups)
	response:=Response{username,Allgroups,nil,nil}
	if state.userisAdminOrNot(username){
		generateHTML(w,response,"index","admins_sidebar","create_group")
	} else {
		http.NotFoundHandler()
	}
}


func (state *RuntimeState) deletegroup_WebpageHandler(w http.ResponseWriter, r *http.Request){
	userInfo, err := authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	response:=Response{username,nil,nil,nil}
	if state.userisAdminOrNot(username){
		generateHTML(w,response,"index","admins_sidebar","delete_group")
	} else {
		http.NotFoundHandler()
	}
}


//requesting access by users to join in groups...
func (state *RuntimeState) request_AccessHandler(w http.ResponseWriter,r *http.Request) {
	userInfo, err := authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	var out map[string][]string
	err = json.NewDecoder(r.Body).Decode(&out)
	if err != nil {
		panic(err)
	}
	log.Println(out)
	fmt.Print(out["groups"])
	err = state.insertInDB(username, out["groups"])
	if err != nil {
		http.Error(w, "oops! an error occured.", http.StatusInternalServerError)
		log.Println(err)
	}
	if state.userisAdminOrNot(username) == true {
		generateHTML(w, Response{UserName:username}, "index","admins_sidebar", "Accessrequestsent")

	} else {
		generateHTML(w, Response{UserName:username}, "index","sidebar","Accessrequestsent")
	}
}


//delete access requests made by user
func (state *RuntimeState) delete_requests(w http.ResponseWriter,r *http.Request) {
	userInfo, err :=authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	var out map[string][]string
	err = json.NewDecoder(r.Body).Decode(&out)
	if err != nil {
		panic(err)
	}
	for _,entry := range out["groups"] {
		err = state.deleteEntryInDB(username,entry)
		if err!=nil{
			log.Println(err)
		}
	}
}

//Parses post info from create group button click.
func (state *RuntimeState) Addmembers_toGroup(w http.ResponseWriter,r *http.Request){
	userInfo, err :=authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	if state.userisAdminOrNot(username) {
		err := r.ParseForm()
		if err != nil {
			panic("Cannot parse form")
		}
		var groupinfo group_info
		groupinfo.groupname = r.PostFormValue("groupname")
		members := r.PostFormValue("members")
		for _, member := range strings.Split(members, ",") {
			groupinfo.memberUid = append(groupinfo.memberUid, member)
			groupinfo.member = append(groupinfo.member, state.Create_UserDN(member))
		}
		err = state.create_Group(groupinfo)
		if err != nil {
			panic(err)
		}
	} else {
		http.NotFoundHandler()
	}

}

//adding members to existing group
func (state *RuntimeState) Addmembers_toexisting(groupinfo group_info)error{
	entry:=state.Create_GroupDN(groupinfo.groupname)
	modify := ldap.NewModifyRequest(entry)
	modify.Add("memberUid", groupinfo.memberUid)
	modify.Add("member",groupinfo.member)
	err := state.target_ldap.Modify(modify)
	if err != nil {
		return err
	}
	return nil
}

//remove members from existing group
func (state *RuntimeState) Deletemembers_fromgroup(groupinfo group_info)error{
	entry:=state.Create_GroupDN(groupinfo.groupname)
	modify:=ldap.NewModifyRequest(entry)
	modify.Delete("memberUid",groupinfo.memberUid)
	modify.Delete("member",groupinfo.member)
	err:=state.target_ldap.Modify(modify)
	if err!=nil{
		return err
	}
	return nil
}

func (state *RuntimeState) exitfrom_group(w http.ResponseWriter,r *http.Request){
	userInfo, err :=authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	var out map[string][]string
	err = json.NewDecoder(r.Body).Decode(&out)
	if err != nil {
		panic(err)
	}
	var groupinfo group_info
	groupinfo.member=append(groupinfo.member,state.Create_UserDN(username))
	groupinfo.memberUid=append(groupinfo.memberUid,username)
	for _,entry := range out["groups"] {
		groupinfo.groupname=entry
		err = state.Deletemembers_fromgroup(groupinfo)
		if err!=nil{
			log.Println(err)
		}
	}
}




func (state *RuntimeState) Addmembers_webpagehandler(w http.ResponseWriter,r *http.Request){


}

//if user is already a member of group or not
func (state *RuntimeState) isGroupMemberorNot(groupname string,username string)bool{

	AllUsersinGroup, err := state.getUsersofaGroup(groupname)
	if err!=nil{
		panic(err)
	}
	for _,entry:=range AllUsersinGroup[0]{
		if entry==username{
			return true
		}
	}
	return false
}

//get description of a group
func (state *RuntimeState) getDescription_value(groupname string)(string,error){
	Base := state.Create_GroupDN(groupname)

	searchRequest := ldap.NewSearchRequest(
		Base,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		"((objectClass=*))",
		[]string{"description"},
		nil,
	)
	sr, err := state.target_ldap.Search(searchRequest)
	if err != nil {
		return "",err
	}
	var result string
	for _,entry:= range sr.Entries{
		result=entry.GetAttributeValue("description")
	}
	return result,nil
}

//User's Pending Actions
func (state *RuntimeState) pending_Actions(w http.ResponseWriter,r *http.Request){
	userInfo, err :=authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	DB_entries,err:=state.getDB_entries()
	if err!=nil{
		panic(err)
	}
	var description string
	var response Response
	response.UserName=username
	for _,entry:=range DB_entries{
		description,err=state.getDescription_value(entry[1])
		if err!=nil{
			panic(err)
		}
		if description=="self-managed"{
			if state.isGroupMemberorNot(entry[1],username){
				response.Pending_actions=append(response.Pending_actions,entry)
			}else{
				continue
			}
		} else if state.isGroupMemberorNot(description,username){
			response.Pending_actions=append(response.Pending_actions,entry)
		}
		continue
	}
	if response.Pending_actions==nil{
		if state.userisAdminOrNot(username) == true {
			generateHTML(w, response, "index", "admins_sidebar", "no_pending_actions")

		} else {
			generateHTML(w, response, "index", "sidebar", "no_pending_actions")
		}
	} else {
		if state.userisAdminOrNot(username) == true {
			generateHTML(w, response, "index", "admins_sidebar", "pending_actions")

		} else {
			generateHTML(w, response, "index", "sidebar", "pending_actions")
		}
	}
}


//Approving
func (state *RuntimeState) approve_handler(w http.ResponseWriter,r *http.Request) {
	userInfo, err :=authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	var out map[string][][]string
	err = json.NewDecoder(r.Body).Decode(&out)
	if err != nil {
		panic(err)
	}
	log.Println(out)
	log.Println(out["groups"])
	var user_pair = out["groups"]
	for _, entry := range user_pair {
		if state.isGroupMemberorNot(entry[1],entry[0]) {
			err=state.deleteEntryInDB(entry[0], entry[1])
			if err!=nil{
				fmt.Println("error me")
				log.Println(err)
			}

		} else if state.entryExistsorNot(entry[0],entry[1]) {
			var groupinfo group_info
			groupinfo.groupname = entry[1]
			groupinfo.memberUid = append(groupinfo.memberUid, entry[0])
			groupinfo.member = append(groupinfo.member, state.Create_UserDN(entry[0]))
			err := state.Addmembers_toexisting(groupinfo)
			if err != nil {
				panic(err)
			}
			err=state.deleteEntryInDB(entry[0],entry[1])
			if err!=nil{
				fmt.Println("here i am")
				log.Println(err)
			}
		}
	}
	go state.send_approve_email(username,out["groups"],r.RemoteAddr,r.UserAgent())
	//generateHTML(w,username,"index","sidebar","Accessrequestsent")
}


//Reject handler
func (state *RuntimeState) reject_handler(w http.ResponseWriter,r *http.Request){
	userInfo, err :=authSource.GetRemoteUserInfo(r)
	if err != nil {
		panic(err)
	}
	if userInfo == nil {
		panic("null userinfo!")
	}
	username := *userInfo.Username
	var out map[string][][]string
	err=json.NewDecoder(r.Body).Decode(&out)
	if err!=nil{
		panic(err)
	}
	log.Println(out)
	fmt.Print(out["groups"])//[[username1,groupname1][username2,groupname2]]
	for _,entry:=range out["groups"]{
		fmt.Println(entry[0],entry[1])
		err=state.deleteEntryInDB(entry[0], entry[1])
		if err!=nil{
			fmt.Println("I am the error")
			log.Println(err)
		}
		//write logs code here
	}
	go state.send_reject_email(username,out["groups"],r.RemoteAddr,r.UserAgent())

}




///Email functions start////

//get email of a user
func (state *RuntimeState) getEmailofaUser(username string)([]string,error){
	var user_email []string
	User_dn := state.Create_UserDN(username)
	searchrequest := ldap.NewSearchRequest(User_dn, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false, "((objectClass=*))", []string{"mail"}, nil)
	result, err := state.target_ldap.Search(searchrequest)
	if err != nil {
		return nil, err
	}
	user_email = append(user_email, result.Entries[0].GetAttributeValues("mail")[0])
	return user_email,nil

}

//get email of all users in the given group
func (state *RuntimeState) getEmailofUsersinGroup(groupname string)([]string,error){
	group_users,err:=state.getUsersofaGroup(groupname)
	if err!=nil{
		log.Println(err)
	}
	var user_email []string
	for _,entry:=range group_users[0]{
		value,err:=state.getEmailofaUser(entry)
		if err != nil {
			return nil, err
		}
		user_email=append(user_email,value[0])

	}
	return user_email, nil
}

//for request access button
func (state *RuntimeState) send_request_email(username string,groupnames []string,remoteAddr string,userAgent string)error{
	for _,entry:=range groupnames{
		description,err:=state.getDescription_value(entry)
		if err!=nil{
			log.Println(err)
			return err
		}
		if description=="self-managed"{
			users_email,err:=state.getEmailofUsersinGroup(entry)
			if err!=nil{
				log.Println(err)
				return err

			}
			state.success_request_email(username,users_email,entry,remoteAddr,userAgent)
		}else{
			users_email,err:=state.getEmailofUsersinGroup(entry)
			if err!=nil{
				log.Println(err)
				return err

			}
			state.success_request_email(username,users_email,entry,remoteAddr,userAgent)
		}
	}
	return nil
}

type mailAttributes struct {
	RequestedUser  string
	OtherUser      string
	Groupname      string
	RemoteAddr     string
	Browser        string
	OS             string
}
////Request Access email  start.....//////

const requestaccess_mailTemplateText = `Subject: Requesting access for group {{.Groupname}}

User {{.RequestedUser}} has requested access for group {{.Groupname}} (from {{.RemoteAddr}} ({{.Browser}} {{.OS}} ))`

//send email for requesting access to a group
func (state *RuntimeState) success_request_email(requesteduser string,users_email []string,groupname string,remoteAddr string,userAgent string) error{
	// Connect to the remote SMTP server.
	c, err := smtp.Dial("smtp.symcpe.net:25")
	if err != nil {
		return err
	}
	defer c.Close()
	// Set the sender and recipient.
	c.Mail("puneeth-noreply@ldap_manager.symcpe.net")
	for _, recipient := range users_email {
		//c.Rcpt("recipient@example.net")
		c.Rcpt(recipient)
	}
	// Send the email body.
	wc, err := c.Data()
	if err != nil {
		log.Fatal(err)
	}
	defer wc.Close()
	//get browser details
	ua := user_agent.New(userAgent)
	uaName, _ := ua.Browser()

	mailData := mailAttributes{
		RequestedUser:    requesteduser,
		Groupname:groupname,
		RemoteAddr:     remoteAddr,
		Browser:        uaName,
		OS:             ua.OS(),
		OtherUser:""}

	templ, err := texttemplate.New("mailbody").Parse(requestaccess_mailTemplateText)
	if err != nil {
		log.Fatal(err)
	}
	err = templ.Execute(wc, mailData)
	if err != nil {
		log.Fatal(err)
	}

	return nil
}
////Request Access email  end.....//////

////Approve email  start.....//////

const requestapprove_mailTemplateText = `Subject: Regarding access for group {{.Groupname}}

User {{.OtherUser}} has Approved access to user {{.RequestedUser}} for group {{.Groupname}} (from {{.RemoteAddr}} ({{.Browser}} {{.OS}} ))`

//send approve email
func (state *RuntimeState) send_approve_email(username string,user_pair [][]string,remoteAddr string,userAgent string) error{
	user_email,err:=state.getEmailofaUser(username)
	if err!=nil{
		log.Println(err)
		return err
	}
	for _,entry:=range user_pair{
		var target_address []string
		target_address=append(target_address,user_email[0])
		requesteduser:=entry[0]
		otheruser_email,err:=state.getEmailofaUser(requesteduser)
		if err!=nil{
			log.Println(err)
			return err
		}
		target_address=append(target_address,otheruser_email[0])
		err=state.approve_request_email(requesteduser,username,target_address,entry[1],remoteAddr,userAgent)
		if err!=nil{
			log.Println(err)
			return err
		}
		target_address=nil
	}
	return nil
}

//for approving requests in pending actions main email function
func (state *RuntimeState) approve_request_email(requesteduser string,otheruser string,users_email []string,groupname string,remoteAddr string,userAgent string) error{
	// Connect to the remote SMTP server.
	c, err := smtp.Dial("smtp.symcpe.net:25")
	if err != nil {
		return err
	}
	defer c.Close()
	// Set the sender and recipient.
	c.Mail("puneeth-noreply@ldap_manager.symcpe.net")
	for _, recipient := range users_email {
		//c.Rcpt("recipient@example.net")
		c.Rcpt(recipient)
	}
	// Send the email body.
	wc, err := c.Data()
	if err != nil {
		log.Fatal(err)
	}
	defer wc.Close()
	//get browser details
	ua := user_agent.New(userAgent)
	uaName, _ := ua.Browser()

	mailData := mailAttributes{
		RequestedUser:    requesteduser,
		Groupname:groupname,
		RemoteAddr:     remoteAddr,
		Browser:        uaName,
		OS:             ua.OS(),
		OtherUser:otheruser}

	templ, err := texttemplate.New("mailbody").Parse(requestapprove_mailTemplateText)
	if err != nil {
		log.Fatal(err)
	}
	err = templ.Execute(wc, mailData)
	if err != nil {
		log.Fatal(err)
	}

	return nil
}
////Approve email  end.....//////

////Reject email  start.....//////
const requestreject_mailTemplateText = `Subject: Regarding access for group {{.Groupname}}

User {{.OtherUser}} has Rejected access to user {{.RequestedUser}} for group {{.Groupname}} (from {{.RemoteAddr}} ({{.Browser}} {{.OS}} ))`

//send reject email
func (state *RuntimeState) send_reject_email(username string,user_pair [][]string,remoteAddr string,userAgent string) error{
	user_email,err:=state.getEmailofaUser(username)
	if err!=nil{
		log.Println(err)
		return err
	}
	for _,entry:=range user_pair{
		var target_address []string
		target_address=append(target_address,user_email[0])
		requesteduser:=entry[0]
		otheruser_email,err:=state.getEmailofaUser(requesteduser)
		if err!=nil{
			log.Println(err)
			return err
		}
		target_address=append(target_address,otheruser_email[0])
		err=state.reject_request_email(requesteduser,username,target_address,entry[1],remoteAddr,userAgent)
		if err!=nil{
			log.Println(err)
			return err
		}
		target_address=nil
	}
	return nil
}


func (state *RuntimeState) reject_request_email(requesteduser string,otheruser string,users_email []string,groupname string,remoteAddr string,userAgent string) error{
	// Connect to the remote SMTP server.
	c, err := smtp.Dial("smtp.symcpe.net:25")
	if err != nil {
		return err
	}
	defer c.Close()
	// Set the sender and recipient.
	c.Mail("puneeth-noreply@ldap_manager.symcpe.net")
	for _, recipient := range users_email {
		//c.Rcpt("recipient@example.net")
		c.Rcpt(recipient)
	}
	// Send the email body.
	wc, err := c.Data()
	if err != nil {
		log.Fatal(err)
	}
	defer wc.Close()
	//get browser details
	ua := user_agent.New(userAgent)
	uaName, _ := ua.Browser()

	mailData := mailAttributes{
		RequestedUser:    requesteduser,
		Groupname:groupname,
		RemoteAddr:     remoteAddr,
		Browser:        uaName,
		OS:             ua.OS(),
		OtherUser:otheruser}

	templ, err := texttemplate.New("mailbody").Parse(requestreject_mailTemplateText)
	if err != nil {
		log.Fatal(err)
	}
	err = templ.Execute(wc, mailData)
	if err != nil {
		log.Fatal(err)
	}

	return nil
}

///// reject email end/////

/// Email function end////

func main() {
	flag.Parse()

	state, err := loadConfig(*configFilename)
	if err != nil {
		panic(err)
	}
	var openidConfigFilename = state.Config.Base.OpenIDCConfigFilename //"/etc/openidc_config_keymaster.yml"

	// if you alresy use the context:
	simpleOidcAuth, err := authhandler.NewSimpleOIDCAuthFromConfig(&openidConfigFilename, nil)
	if err != nil {
		panic(err)
	}
	authSource = simpleOidcAuth


	//Parsing Source LDAP URL, establishing connection and binding user.
	Source_LdapUrl, err := authutil.ParseLDAPURL(state.Config.SourceLDAP.LDAPTargetURLs)

	state.source_ldap, _, err = GetLDAPConnection(*Source_LdapUrl, ldapTimeoutSecs, nil)
	if err != nil {
		panic(err)
	}

	timeout := time.Duration(time.Duration(ldapTimeoutSecs) * time.Second)
	state.source_ldap.SetTimeout(timeout)
	state.source_ldap.Start()

	err = state.source_ldap.Bind(state.Config.SourceLDAP.BindUsername, state.Config.SourceLDAP.BindPassword)

	if err != nil {
		panic(err)
	}

	//Parsing Target LDAP, establishing connection and binding user.
	Target_LdapUrl, err := authutil.ParseLDAPURL(state.Config.TargetLDAP.LDAPTargetURLs)

	state.target_ldap, _, err = GetLDAPConnection(*Target_LdapUrl, ldapTimeoutSecs, nil)
	if err != nil {
		panic(err)
	}
	state.target_ldap.SetTimeout(timeout)
	state.target_ldap.Start()

	err = state.target_ldap.Bind(state.Config.TargetLDAP.BindUsername, state.Config.TargetLDAP.BindPassword)

	if err != nil {
		panic(err)
	}

	http.HandleFunc("/allgroups", state.GetallGroups_Handler)
	http.HandleFunc("/allusers", state.GetallUsers_Handler)
	http.HandleFunc("/user_groups/", state.GetGroupsofUser_Handler)
	http.HandleFunc("/group_users/", state.GetUsersinGroup_Handler)


	http.Handle("/create_group", simpleOidcAuth.Handler(http.HandlerFunc(state.creategroup_WebpageHandler)))
	http.Handle("/delete_group", simpleOidcAuth.Handler(http.HandlerFunc(state.deletegroup_WebpageHandler)))
	http.Handle("/create_group/",simpleOidcAuth.Handler(http.HandlerFunc(state.createGroup_handler)))
	http.Handle("/delete_group/",simpleOidcAuth.Handler(http.HandlerFunc(state.deleteGroup_handler)))

	http.Handle("/requestaccess",simpleOidcAuth.Handler(http.HandlerFunc(state.request_AccessHandler)))
	http.Handle("/index.html", simpleOidcAuth.Handler(http.HandlerFunc(state.Index_Handler)))
	http.Handle("/group/",simpleOidcAuth.Handler(http.HandlerFunc(state.Group_Handler)))
	http.Handle("/mygroups/",simpleOidcAuth.Handler(http.HandlerFunc(state.MyGroups_Handler)))
	http.Handle("/pending-actions",simpleOidcAuth.Handler(http.HandlerFunc(state.pending_Actions)))
	http.Handle("/pending-requests",simpleOidcAuth.Handler(http.HandlerFunc(state.pending_Requests)))
	http.Handle("/deleterequests",simpleOidcAuth.Handler(http.HandlerFunc(state.delete_requests)))
	http.Handle("/exitgroup",simpleOidcAuth.Handler(http.HandlerFunc(state.exitfrom_group)))

	http.Handle("/approve-request",simpleOidcAuth.Handler(http.HandlerFunc(state.approve_handler)))
	http.Handle("/reject-request",simpleOidcAuth.Handler(http.HandlerFunc(state.reject_handler)))

	http.HandleFunc("/addmembers",state.Addmembers_webpagehandler)
	http.HandleFunc("/addmembers/",state.Addmembers_toGroup)

	fs:=http.FileServer(http.Dir("templates"))
	http.Handle("/css/",fs)
	http.Handle("/js/",fs)
	http.Handle("/images/",fs)
	http.Handle("/fonts/",fs)
	log.Fatal(http.ListenAndServeTLS(state.Config.Base.HttpAddress,state.Config.Base.TLSCertFilename,state.Config.Base.TLSKeyFilename, nil))
}

