hostname="128.237.217.178"
openssl genrsa -out $hostname.key 2048
openssl req -new -x509 -key $hostname.key -out $hostname.cert -days 3650 -subj /CN=$hostname
