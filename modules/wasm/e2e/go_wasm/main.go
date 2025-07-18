package main

func main() {

}

func callfunc(arg string) {
	// println("adding two numbers:", add(2, 3)) // expecting 5
}

//export multiply
func multiply(x, y int) int {
	return x * y
}
