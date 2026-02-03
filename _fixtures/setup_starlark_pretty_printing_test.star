def format_bytes(arg):
	return "bytes start with " + str(arg[0]) + " and end with " + str(arg[-1])

PrettyPrint["[]uint8"] = format_bytes
